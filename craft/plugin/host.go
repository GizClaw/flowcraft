package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/hooks"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/craft/plugin/server"
)

// NewSourceFunc builds one plugin's tool source. Tests inject fakes;
// production uses the wire source factory, which spawns the plugin process
// and waits for it to dial the socket craft opened for it.
//
// The generation is the process this source is being opened for. It is
// minted before the environment hook runs, so a source and the grants its
// process was started with belong to each other by construction.
type NewSourceFunc func(
	ctx context.Context,
	entry Entry,
	generation uint64,
	env map[string]string,
	dataDir string,
) (Source, error)

// DefaultEnableTimeout bounds the readiness wait of an enable: how long
// [Host.SetEnabled] waits for a plugin to say Ready on its connection
// before it stops waiting and records the plugin as enabled anyway.
const DefaultEnableTimeout = 10 * time.Second

// DefaultDrainTimeout bounds how long a stop waits for one plugin's
// in-flight calls before its source is closed anyway. It is
// [HostOptions.DrainTimeout]'s zero value, and the bound every wait in
// this package takes when nothing else is configured: an unbounded one
// would let a single hung call keep a plugin — or the whole host —
// from ever stopping.
const DefaultDrainTimeout = 30 * time.Second

// HostOptions configures a plugin Host.
type HostOptions struct {
	Store *Store
	// Server is the craft half of the plugin wire: the primitive set every
	// plugin is served and the listener each process's socket comes from.
	// A host without one can still be given a NewSource of its own, which is
	// how tests replace the transport; enabling a plugin with neither is an
	// error rather than a plugin that silently publishes nothing.
	Server       *server.Server
	NewSource    NewSourceFunc
	Env          map[string]string
	DrainTimeout time.Duration
	// EnableTimeout bounds how long [Host.SetEnabled] waits for a plugin
	// to come up. Zero means [DefaultEnableTimeout]; a negative value
	// does not wait at all, which is what every enable did before the
	// wait existed.
	EnableTimeout time.Duration
}

// Host owns the enabled plugin processes: one tool source per plugin,
// shared by every runtime of a Craft.
type Host struct {
	store         *Store
	server        *server.Server
	newSource     NewSourceFunc
	env           map[string]string
	drainTimeout  time.Duration
	enableTimeout time.Duration
	tools         *ToolSet
	// awaitingReady lists the plugins whose enable is in flight, so a
	// terminal loss that arrives while one waits is held for the call
	// that is asking for the plugin instead of being reported from under
	// it (see notifyLost and finishEnable).
	awaitingReady map[string]bool

	mu      sync.Mutex
	started bool
	closed  bool
	// live is what the host knows about each running plugin: the entry
	// its process was started from, the generation that process belongs
	// to, and the source itself. A plugin the record says is enabled but
	// whose process gave up is not live — the loss was reported, and the
	// record is the application's to change.
	live map[string]livePlugin
	// nextGeneration numbers the processes this host starts. Everything
	// derived from a process — the connection opened for it and the grants
	// it is served with — is keyed by its generation, so nothing derived
	// from a dead process outlives it.
	nextGeneration uint64
	// entryLocks serializes start and stop for one plugin id, so an
	// enable racing a disable (or a start racing Close) cannot leave the
	// store saying one thing and the process table another.
	entryLocks map[string]*sync.Mutex
	envHook    func(Entry, uint64, map[string]string) error
	stopped    func(string)
	// deferredLoss lists the plugins whose terminal loss arrived while an
	// enable was in flight. The enable in flight narrates its plugin's
	// failure, so the loss waits for it: reported if the enable stands
	// and its own report if the enable rolls back.
	deferredLoss map[string]bool
	// reporting holds, per plugin, the generation whose stop is being
	// narrated to the subscribers right now. A process that gave up on
	// its own and the host stopping that same process are one event —
	// the process is gone either way — so exactly one of the two
	// narrates it: whoever records it first takes the generation, and
	// the other side stays silent about that stop while its own half of
	// the work still runs (see [Host.notifyLost] and
	// [Host.stopEntryLocked]). A stop of a later process is a different
	// event and narrates on its own, which is why the claim is keyed by
	// generation rather than by plugin.
	reporting map[string]uint64
	// lost lists the plugins whose process gave up on its own. The entry
	// keeps its state and its tools — the loss was reported, and whether
	// the plugin stays enabled is the application's decision — but the
	// source behind it has no process and no reconnection coming, so an
	// enable of this plugin replaces it instead of finding it "already
	// running".
	lost map[string]bool
	// stopHooks are the additive OnStopped subscriptions. They are
	// deliberately separate from stopped above: craft's token
	// revocation must not depend on what an application subscribes to.
	stopHooks   []stopHook
	stopHookSeq uint64

	// watchCancel, watchUnsubscribe and watchDone belong to the revision
	// watcher that keeps the running plugins in step with the store (see
	// [Host.watchRevisions]).
	watchCancel      context.CancelFunc
	watchUnsubscribe func()
	watchDone        chan struct{}
}

// stopHook is one OnStopped subscription. Subscriptions are identified
// by sequence number because func values cannot be compared.
type stopHook struct {
	id uint64
	fn func(string, StopReason)
}

// livePlugin is one running plugin's process: the entry it was started
// from, the generation that process belongs to, and the source the host
// opened for it.
//
// The entry is kept rather than re-read, because a restart has to be
// planned against what the process was: [Host.startEntryLocked] hands
// the generation to the environment hook, which is where the token for
// this process is minted, so a process and the grants it was started
// with belong to each other by construction.
type livePlugin struct {
	entry      Entry
	generation uint64
	source     Source
}

// NewHost opens a plugin host over a store.
func NewHost(opts HostOptions) (*Host, error) {
	if opts.Store == nil {
		return nil, errdefs.Validationf("plugin host: Store is required")
	}
	drain := opts.DrainTimeout
	if drain <= 0 {
		drain = DefaultDrainTimeout
	}
	enable := opts.EnableTimeout
	if enable == 0 {
		enable = DefaultEnableTimeout
	}
	env := make(map[string]string, len(opts.Env))
	for key, value := range opts.Env {
		env[key] = value
	}
	host := &Host{
		store:         opts.Store,
		server:        opts.Server,
		newSource:     opts.NewSource,
		env:           env,
		drainTimeout:  drain,
		enableTimeout: enable,
		tools:         newToolSet(drain),
		live:          make(map[string]livePlugin),
		entryLocks:    make(map[string]*sync.Mutex),
		awaitingReady: make(map[string]bool),
		deferredLoss:  make(map[string]bool),
		reporting:     make(map[string]uint64),
		lost:          make(map[string]bool),
	}
	if host.newSource == nil {
		host.newSource = host.wireSource
	}
	return host, nil
}

// Store returns the underlying store.
func (h *Host) Store() *Store { return h.store }

// SetServer installs the wire server that opens a socket per plugin
// process. It must be called before Start: a start replays the enabled
// plugins, and each of those needs its socket to exist by the time its
// process spawns.
func (h *Host) SetServer(server *server.Server) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return
	}
	h.server = server
}

// SetPluginHooks installs the per-plugin env injector and the
// stopped-plugin callback. It must be called before Start. An
// application that wants a stop notification of its own subscribes
// with [Host.OnStopped] instead: that seam is additive, so using it
// cannot displace this wiring.
//
// The injector is handed the generation of the process it is preparing:
// it is where an application mints what that process is allowed to do
// (craft mints the plugin's host token there), and a generation that
// changes is what tells it the grants of the process being replaced have
// to go with it.
func (h *Host) SetPluginHooks(
	env func(Entry, uint64, map[string]string) error,
	stopped func(string),
) {
	h.mu.Lock()
	h.envHook = env
	h.stopped = stopped
	h.mu.Unlock()
}

// StopReason explains why a plugin stopped.
type StopReason string

const (
	// StopRequested is a stop the application asked for: disabling the
	// plugin, uninstalling it, or closing the host.
	StopRequested StopReason = "requested"

	// StopFailed is a process the host did not stop: it exited on its own,
	// or its connection ended without a stop from the host. The entry
	// keeps its state — enabled, with its tools still projected — and the
	// application decides what the loss means.
	StopFailed StopReason = "failed"

	// StopRolledBack is a stop that undid an enable: the host stopped a
	// process it had just started because the enable could not be
	// completed — the plugin never came up (the error [Host.SetEnabled]
	// returned says why), or the state write that would have recorded
	// the enable failed. The entry is left disabled, so the application
	// has nothing to correct.
	StopRolledBack StopReason = "rolled_back"
)

// OnStopped subscribes to plugin stops. The callback receives the
// plugin id and why it stopped: StopRequested for a stop the
// application asked for, after the process is gone and its tools have
// left the shared set; StopRolledBack for an enable the host undid
// because the plugin could not come up; StopFailed for a process that
// stopped on its own and could not be reconnected, with the entry and
// its tools left as they are so the application decides what the loss
// means.
//
// Multiple subscriptions are allowed and the returned function
// unsubscribes this one; calling it more than once is safe. Stops
// happen whether or not anyone subscribed — craft's own wiring is
// independent of these subscriptions. A callback runs on the goroutine
// that stopped the plugin (or, for StopFailed, on the one that noticed
// the process is gone), so it should return promptly and must not call
// back into the host for the same plugin: that stop already holds the
// plugin's entry lock.
func (h *Host) OnStopped(fn func(id string, reason StopReason)) func() {
	if fn == nil {
		return func() {}
	}
	h.mu.Lock()
	h.stopHookSeq++
	sub := stopHook{id: h.stopHookSeq, fn: fn}
	h.stopHooks = append(h.stopHooks, sub)
	h.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			for i, candidate := range h.stopHooks {
				if candidate.id == sub.id {
					h.stopHooks = append(h.stopHooks[:i], h.stopHooks[i+1:]...)
					return
				}
			}
		})
	}
}

// stopSubscribers snapshots the current subscriptions, so notifications
// run without the host lock held.
func (h *Host) stopSubscribers() []func(string, StopReason) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]func(string, StopReason), 0, len(h.stopHooks))
	for _, sub := range h.stopHooks {
		out = append(out, sub.fn)
	}
	return out
}

// notifyStopped reports a stop the host performed. The internal hook
// runs first and unconditionally — it is what revokes the plugin's host
// tokens — and the subscribers hear about it afterwards.
func (h *Host) notifyStopped(id string, reason StopReason) {
	h.runStoppedHook(id)
	for _, fn := range h.stopSubscribers() {
		fn(id, reason)
	}
}

// runStoppedHook runs the internal stopped hook without reporting a stop
// to the subscribers. It is the half of a stop that is not an
// application-facing event: the token revocation of a process the host
// let go of, whether or not a subscriber should learn about it.
func (h *Host) runStoppedHook(id string) {
	h.mu.Lock()
	stopped := h.stopped
	h.mu.Unlock()
	if stopped != nil {
		stopped(id)
	}
}

// notifyLost reports a process that stopped on its own and whose
// reconnect gave up. Only subscribers hear about it: the host did not
// stop the plugin — the entry keeps its state and its tools — so the
// internal hook (and with it token revocation) does not run for the loss
// itself: it runs when the source is finally let go of, which is an
// application-requested stop, a rollback, or the replacement an enable
// opens. A loss that races the host's own close is
// not reported, and neither is the loss of a plugin whose source the
// host no longer tracks: that plugin is one no one is waiting on,
// because the application (or a rollback) already took it away.
//
// A loss that arrives while an enable is in flight is held rather than
// reported or dropped: the enable in flight is what narrates its
// plugin's failure, so the loss waits for [Host.finishEnable] to decide
// whether that call reported it (a rollback) or whether the application
// still has to hear it.
//
// A loss that arrives while the host is already stopping this process is
// not reported: that stop is the report for it. The claim is taken under
// the host lock by whichever side gets there first, so one process's
// stop reaches the subscribers once (see
// [Host.claimStopReportLocked]).
func (h *Host) notifyLost(id string) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	live, tracked := h.live[id]
	if !tracked {
		h.mu.Unlock()
		return
	}
	if h.awaitingReady[id] {
		h.deferredLoss[id] = true
		h.mu.Unlock()
		return
	}
	if !h.claimStopReportLocked(id, live.generation) {
		// The host is stopping this very process right now, and that
		// stop is what the application is being told about; narrating
		// the loss as well would report one stop twice.
		h.mu.Unlock()
		return
	}
	h.lost[id] = true
	h.mu.Unlock()
	for _, fn := range h.stopSubscribers() {
		fn(id, StopFailed)
	}
	h.mu.Lock()
	h.releaseStopReportLocked(id, live.generation)
	h.mu.Unlock()
}

// claimStopReportLocked takes the narration of one process's stop and
// reports whether this call got it. The claim is per plugin and
// generation: a loss and the host's own stop of the same process are one
// event, while a later process stops as its own event and narrates on
// its own. Callers hold the host lock.
func (h *Host) claimStopReportLocked(id string, generation uint64) bool {
	if h.reporting[id] == generation {
		return false
	}
	h.reporting[id] = generation
	return true
}

// releaseStopReportLocked gives back a claim, but only the one this
// generation took: a claim that has since been replaced belongs to
// another process's stop. Callers hold the host lock.
func (h *Host) releaseStopReportLocked(id string, generation uint64) {
	if h.reporting[id] == generation {
		delete(h.reporting, id)
	}
}

// finishEnable ends one enable's wait. A loss that arrived while the
// enable was in flight was held for it, and report says what becomes of
// it: the loss is the application's to hear when the enable stands —
// recorded enabled, with a process that gave up and no other report —
// and stays quiet when the enable rolled back (that call is the report)
// or when the host is closing (a loss that races the host's own close is
// not reported either).
//
// The enable in flight ends here rather than after the caller returns,
// so the flag and the held loss are resolved under one lock: a loss
// arriving after it, or during the delivery below, is reported by
// [Host.notifyLost] itself.
func (h *Host) finishEnable(id string, report bool) {
	h.mu.Lock()
	delete(h.awaitingReady, id)
	held := h.deferredLoss[id]
	delete(h.deferredLoss, id)
	deliver := held && report && !h.closed
	if deliver {
		h.lost[id] = true
	}
	h.mu.Unlock()
	if !deliver {
		return
	}
	for _, fn := range h.stopSubscribers() {
		fn(id, StopFailed)
	}
}

// Tools returns the shared tool aggregator.
func (h *Host) Tools() tool.Source { return h.tools }

// ToolSet returns the concrete aggregator.
func (h *Host) ToolSet() *ToolSet { return h.tools }

// Entries returns every valid scanned plugin.
func (h *Host) Entries() ([]Entry, error) { return h.store.Entries() }

// CallTool invokes one tool over a running plugin's connection.
func (h *Host) CallTool(
	ctx context.Context,
	pluginID, toolName string,
	args any,
) (json.RawMessage, error) {
	h.mu.Lock()
	live, running := h.live[pluginID]
	h.mu.Unlock()
	if !running {
		return nil, errdefs.NotAvailablef(
			"plugin host: plugin %q is not running", pluginID)
	}
	caller, ok := live.source.(Caller)
	if !ok {
		return nil, errdefs.NotAvailablef(
			"plugin host: plugin %q does not support direct calls", pluginID)
	}
	return caller.CallTool(ctx, toolName, args)
}

// Revision delegates to the store.
func (h *Host) Revision() uint64 { return h.store.Revision() }

// Subscribe delegates to the store.
func (h *Host) Subscribe(fn func()) func() { return h.store.Subscribe(fn) }

// List returns the UI-facing plugin summaries.
func (h *Host) List() ([]Summary, error) { return h.store.List() }

// KV returns the plugin's namespaced key/value store.
func (h *Host) KV(id string) (*KV, error) { return h.store.KV(id) }

// Start launches every enabled plugin. Individual plugin failures are
// aggregated; the remaining plugins still start.
//
// Start does not wait for plugins to come up and does not undo the state
// they were enabled with: it replays what a previous session recorded,
// and an application's startup must not depend on a plugin's process.
// A plugin that cannot come up is reported through [Host.OnStopped]
// instead; [Host.SetEnabled] is the call that waits, because an
// application asking for this plugin now is entitled to know whether it
// arrived.
func (h *Host) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errdefs.NotAvailablef("plugin host: closed")
	}
	first := !h.started
	h.started = true
	h.mu.Unlock()
	if first {
		// From here on the host follows the store: whatever changes it —
		// this host's own operations, an application talking to the
		// store directly — the running plugins are brought back in step
		// with the record (see [Host.watchRevisions]).
		h.watchRevisions()
	}
	entries, err := h.store.Enabled()
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if err := h.startEntry(ctx, entry); err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", entry.ID, err))
		}
	}
	return errors.Join(errs...)
}

// SetEnabled starts or stops one plugin and bumps the store revision.
//
// Starting waits for the plugin to come up, bounded by
// [HostOptions.EnableTimeout], so an enable reports a plugin that cannot
// come up at all — a command that is gone or not executable, a wire
// version craft cannot speak, a declaration craft refuses — instead of
// recording an enable whose process never arrives. Such an enable is
// rolled back: the process is stopped and the entry is left disabled. An
// entry a previous session had recorded as enabled loses that record
// along with the process, because a record that says enabled about a
// plugin with no process is the disagreement this wait exists to prevent.
// A plugin that is merely not there yet — a process still starting, one
// that has not dialed the socket — is not a failure of enable: the wait
// gives up at the timeout and the entry is recorded enabled.
//
// Enabling a plugin that is still running starts nothing, and enabling
// one whose source gave up is a start: the process behind that source is
// gone (the application heard about it through [Host.OnStopped]), so the
// dead source is replaced and this call reports whether the replacement
// came up. A loss that arrives while the wait runs is that call's to
// narrate: the rollback is its report, and an enable that stands hands
// the loss on once it ends.
//
// The wait runs while the plugin's entry lock is held, so a disable or a
// Close racing this enable waits for it, bounded by that timeout.
func (h *Host) SetEnabled(
	ctx context.Context,
	id string,
	enabled bool,
) error {
	entry, ok := h.store.Entry(id)
	if !ok {
		return errdefs.NotFoundf("plugin host: plugin %q not found", id)
	}
	// The store refuses the same thing, but it refuses it after this
	// call started a process: a plugin whose tool namespace another
	// plugin claims must not be brought up at all.
	if enabled && entry.Conflict != "" {
		return errdefs.Conflictf(
			"plugin host: plugin %q cannot be enabled: %s", id, entry.Conflict)
	}
	// The whole toggle — the process and the record that describes it —
	// is serialized with every other toggle of this plugin. Holding the
	// lock only while the process is spawned is not enough: the state
	// write that follows it would still be able to overtake a disable
	// that raced it and leave the two disagreeing.
	lock := h.entryLock(id)
	lock.Lock()
	defer lock.Unlock()
	if enabled {
		// A loss that arrives while this enable waits is held for it: the
		// call in flight is what narrates its plugin's failure, so what
		// becomes of the loss is decided here — the rollback below is its
		// report, while an enable that stands hands it on to the
		// application (see finishEnable).
		stands := false
		h.setAwaitingReady(id)
		defer func() { h.finishEnable(id, stands) }()
		source, err := h.startEntryLocked(ctx, entry)
		if err != nil {
			return err
		}
		if err := h.awaitReady(ctx, source); err != nil {
			if !errdefs.IsValidation(err) {
				// Not up yet, not hopeless: the source keeps
				// reconnecting, the entry is enabled as asked, and a loss
				// that turns out to be terminal reaches the application
				// through [Host.OnStopped].
				telemetry.WarnErr(ctx, "plugin host: plugin is not ready yet", err,
					attribute.String("plugin", id))
			} else {
				// Terminal: no enable can bring this plugin up, so the start
				// is undone rather than recorded. An entry a previous
				// session recorded as enabled goes with it — the record
				// would otherwise say enabled about a plugin with no
				// process, the disagreement this wait exists to prevent.
				_ = h.stopEntryLocked(ctx, id, StopRolledBack)
				if entry.Enabled {
					// The record goes with the process, and a write that
					// fails is reported alongside the reason the enable
					// was undone: nothing else repairs that
					// disagreement — the failed write bumps no revision
					// for a reconcile pass to visit — so swallowing it
					// would leave the application with an entry that
					// says enabled about a plugin nothing can start.
					return h.abandonLocked(ctx, id, err)
				}
				return err
			}
		}
		if err := h.store.SetEnabled(id, true); err != nil {
			// The process is up but the state write failed, which would
			// leave the recorded state saying "disabled" about a running
			// plugin. Stopping it again keeps the two agreeing: the error
			// the caller sees means nothing changed.
			_ = h.stopEntryLocked(ctx, id, StopRolledBack)
			return err
		}
		// The plugin is up and the record says so: the enable stands, and
		// anything the wait collected is a loss the application has not
		// heard about yet.
		stands = true
		return nil
	}
	// Disabling is a change like the ones [Host.changeLocked] runs: the
	// record has to end up describing what the host holds. A stop that
	// could not take the process out of the way closed its source
	// anyway, so the record saying enabled goes with it — and nothing
	// else repairs that disagreement: the write that failed bumps no
	// revision, so no reconcile pass visits a plugin that is no longer
	// live, and the application would see a plugin whose tools are gone
	// while its record says it is running.
	if err := h.stopEntryLocked(ctx, id, StopRequested); err != nil {
		return h.abandonLocked(ctx, id, err)
	}
	if err := h.store.SetEnabled(id, false); err != nil {
		// The process is gone and the record is still the old one. The
		// same compensation applies: the write is attempted again, and a
		// record that cannot be written either way is reported alongside
		// the reason the disable failed.
		return h.abandonLocked(ctx, id, err)
	}
	return nil
}

// setAwaitingReady records that one plugin's enable is in flight. The
// end of that enable is [Host.finishEnable], which clears it.
func (h *Host) setAwaitingReady(id string) {
	h.mu.Lock()
	h.awaitingReady[id] = true
	h.mu.Unlock()
}

// awaitReady waits for the source an enable just opened, bounded by the
// host's enable timeout. A nil error means there is nothing to wait for
// — this call opened no source, the source cannot report readiness, or
// the host is configured not to wait — or that the wait was satisfied.
// A non-nil error is the source's report, and its class decides what the
// caller does with it.
func (h *Host) awaitReady(ctx context.Context, source Source) error {
	if source == nil || h.enableTimeout <= 0 {
		return nil
	}
	waiter, ok := source.(Readiness)
	if !ok {
		return nil
	}
	// The context and the timeout carry the same bound: the context is
	// what keeps a source with several servers from spending the bound
	// once per server.
	waitCtx, cancel := context.WithTimeout(ctx, h.enableTimeout)
	defer cancel()
	return waiter.WaitReady(waitCtx, h.enableTimeout)
}

// Uninstall stops one plugin and then removes it through the store.
// Stopping comes first and its failure aborts the uninstall: removing a
// directory out from under a running plugin would take the files its
// process still reads and leaves the drain waiting for work that will
// never finish.
//
// It is one of the plugin's migrations, so it holds the entry lock and
// runs in the order the others do (see [Host.changeLocked]): what the
// uninstall would be refused for is asked before the stop — killing the
// process of a builtin plugin (or of an id that does not exist) and only
// then discovering the uninstall cannot happen leaves the record saying
// enabled about a plugin whose process is gone — and a store that
// refuses after the stop leaves a plugin that was running running
// again. The lock is what keeps an enable from starting a process while
// the store removes the directory that process is served from.
func (h *Host) Uninstall(
	ctx context.Context,
	id string,
	opts UninstallOptions,
) error {
	lock := h.entryLock(id)
	lock.Lock()
	defer lock.Unlock()
	if err := h.store.CheckUninstall(id); err != nil {
		return err
	}
	wasRunning := h.isLive(id)
	if err := h.stopEntryLocked(ctx, id, StopRequested); err != nil {
		// The process could not be taken out of the way in time; it was
		// closed anyway, so the record that says enabled goes with it.
		return h.abandonLocked(ctx, id, err)
	}
	if err := h.store.Uninstall(ctx, id, opts); err != nil {
		// The plugin is still installed — the store refused — so what
		// was running goes back up: the caller sees why the uninstall
		// failed, and a plugin that was running is running again.
		return h.resumeLocked(ctx, id, wasRunning, err)
	}
	return nil
}

// Install installs a plugin from a directory and keeps its process in
// step with what was installed. A zip package is unpacked by
// [Store.InstallZip], which is the store's path and touches no process.
//
// The store pipeline is the one [Store.Install] runs — the same checks,
// the same snapshot, the same restore on failure. What this method adds
// is the process side of it: an install replaces the directory a running
// plugin's process reads, so a running plugin is stopped for the change
// and started again afterwards, from what the store now holds. See
// [Host.changeLocked] for what happens when either half fails.
//
// An id that was not installed at all is recorded disabled once the
// install succeeded: a change does not turn a plugin on, and the entry a
// fresh id would otherwise carry — a store with no record for it reads
// enabled — would say enabled about a plugin with no process behind it,
// the one state a running host does not leave behind. [Host.SetEnabled]
// is what starts it.
//
// The summary describes what was installed. One kind of failure returns
// it alongside the error: a fresh install that landed but whose
// "disabled" record could not be written — the summary is what is on
// disk, and the error says what could not be recorded about it.
func (h *Host) Install(ctx context.Context, source string) (Summary, error) {
	// The id names the process and the entry lock this install has to
	// keep in step, and it is known only once the source is read.
	summary, err := h.store.Inspect(ctx, source)
	if err != nil {
		return Summary{}, err
	}
	lock := h.entryLock(summary.ID)
	lock.Lock()
	defer lock.Unlock()
	_, installed := h.store.Entry(summary.ID)
	summary, err = h.changeLocked(ctx, summary.ID, func() (Summary, error) {
		return h.store.Install(ctx, source)
	})
	if err != nil {
		return Summary{}, err
	}
	if installed {
		return summary, nil
	}
	if err := h.store.SetEnabled(summary.ID, false); err != nil {
		return summary, fmt.Errorf(
			"plugin host: record the fresh install of %s as disabled: %w",
			summary.ID, err)
	}
	return summary, nil
}

// UpdateFrom updates one installed plugin from its declared update
// source and keeps its process in step with what was installed. The
// update is refused and nothing is installed when the version rules or
// the installer's approval say no (see [Store.UpdateFrom]).
//
// A running plugin is stopped for the update rather than after it: the
// install replaces the directory the process reads. An update that the
// source refuses — the network, the checksum, a version that is not
// newer — starts the installed version again before this call returns,
// so what the refusal costs a running plugin is a restart, not a
// stopped plugin.
func (h *Host) UpdateFrom(
	ctx context.Context,
	id string,
	installer Installer,
) (Summary, error) {
	if _, ok := h.store.Entry(id); !ok {
		return Summary{}, errdefs.NotFoundf(
			"plugin host: plugin %q not found", id)
	}
	lock := h.entryLock(id)
	lock.Lock()
	defer lock.Unlock()
	return h.changeLocked(ctx, id, func() (Summary, error) {
		return h.store.UpdateFrom(ctx, id, installer)
	})
}

// Rollback restores the last snapshot of one plugin and keeps its
// process in step with it.
//
// A rollback whose version cannot come up is the one change this file
// cannot undo: the swap consumes the version it replaced, so the plugin
// is left stopped with its record disabled rather than saying enabled
// about a process that is not there. The error says what failed;
// installing a package is what repairs the plugin.
func (h *Host) Rollback(ctx context.Context, id string) (Summary, error) {
	if _, ok := h.store.Entry(id); !ok {
		return Summary{}, errdefs.NotFoundf(
			"plugin host: plugin %q not found", id)
	}
	lock := h.entryLock(id)
	lock.Lock()
	defer lock.Unlock()
	return h.changeLocked(ctx, id, func() (Summary, error) {
		return h.store.Rollback(ctx, id)
	})
}

// changeLocked runs one store-side change of an installed plugin with
// the plugin's process kept in step with it. Callers hold the plugin's
// entry lock.
//
// The protocol is stop → change → start, and each half makes up for the
// other's failure:
//
//   - The process is stopped before the change, because the change
//     replaces or removes the directory the process reads.
//   - A change the store refused leaves the version that was installed
//     in place, so the plugin is started again from it: the caller sees
//     the store's error and a plugin that was running is running again.
//   - A change that landed but whose version cannot come up is undone
//     where the store can undo it (the snapshot a rollback restores),
//     and the version that ran before is started again. The caller sees
//     why the new version failed.
//   - A plugin no version can run is left stopped with its record
//     disabled: "enabled" about a plugin with no process is the one
//     state nothing else here allows an application to see.
//
// A plugin that was not running is not started by a change: an update
// does not turn a plugin on. That includes a plugin whose process gave
// up on its own — the loss was reported and the record is the
// application's to change, so a change does not silently undo the
// application's decision to leave it as it is.
func (h *Host) changeLocked(
	ctx context.Context,
	id string,
	change func() (Summary, error),
) (Summary, error) {
	wasRunning := h.isLive(id)
	if err := h.stopEntryLocked(ctx, id, StopRequested); err != nil {
		// The process could not be taken out of the way: a call did not
		// finish in time, and the stop closed the source anyway. Nothing
		// has changed — changing files under a live process is what the
		// stop exists to prevent — but the process is gone, so the
		// record saying enabled goes with it.
		return Summary{}, h.abandonLocked(ctx, id, err)
	}
	summary, err := change()
	if err != nil {
		return Summary{}, h.resumeLocked(ctx, id, wasRunning, err)
	}
	if err := h.startAgain(ctx, id, wasRunning); err != nil {
		return Summary{}, h.undoLocked(ctx, id, wasRunning, err)
	}
	return summary, nil
}

// resumeLocked starts the plugin again from what the store still holds,
// for a change that was refused. The cause is what the caller is told
// when the plugin is running again; a plugin that cannot be started at
// all is a plugin that cannot be enabled, and the cause says so.
func (h *Host) resumeLocked(
	ctx context.Context,
	id string,
	wasRunning bool,
	cause error,
) error {
	if err := h.startAgain(ctx, id, wasRunning); err != nil {
		return h.abandonLocked(ctx, id, errors.Join(cause, err))
	}
	return cause
}

// undoLocked puts the version that ran before the change back and starts
// it, for a change that landed but whose version cannot come up. A
// change the store cannot undo — a rollback consumed the version it
// replaced — leaves the plugin abandoned instead.
func (h *Host) undoLocked(
	ctx context.Context,
	id string,
	wasRunning bool,
	cause error,
) error {
	if _, err := h.store.Rollback(ctx, id); err != nil {
		return h.abandonLocked(ctx, id, errors.Join(cause, err))
	}
	if err := h.startAgain(ctx, id, wasRunning); err != nil {
		return h.abandonLocked(ctx, id, errors.Join(cause, err))
	}
	return cause
}

// abandonLocked is the last resort of a change that left a plugin
// unable to run: its process is gone (the caller stopped it), so the
// record that says enabled goes with it. The cause is returned either
// way; a record that could not be written is reported alongside it.
func (h *Host) abandonLocked(ctx context.Context, id string, cause error) error {
	if err := h.store.SetEnabled(id, false); err != nil {
		return errors.Join(cause, fmt.Errorf(
			"plugin host: %s is stopped; recording it disabled: %w", id, err))
	}
	return cause
}

// startAgain starts the plugin's process from what the store now holds.
// wasRunning is what the caller saw before it stopped the process: a
// plugin that was not running is left that way (see [Host.changeLocked]).
//
// A terminal readiness failure stops the process again and is returned
// as the reason the version cannot run; a version that is merely not up
// yet is left to the source's own reconnection, exactly as an enable
// leaves it.
func (h *Host) startAgain(
	ctx context.Context,
	id string,
	wasRunning bool,
) error {
	if !wasRunning {
		return nil
	}
	entry, ok := h.store.Entry(id)
	if !ok {
		return errdefs.NotFoundf(
			"plugin host: plugin %q is no longer installed", id)
	}
	source, err := h.startEntryLocked(ctx, entry)
	if err != nil {
		return err
	}
	err = h.awaitReady(ctx, source)
	switch {
	case err == nil:
		return nil
	case errdefs.IsValidation(err):
		// Terminal: nothing this version does will bring it up.
		_ = h.stopEntryLocked(ctx, id, StopRolledBack)
		return err
	default:
		telemetry.WarnErr(ctx, "plugin host: plugin is not ready yet", err,
			attribute.String("plugin", id))
		return nil
	}
}

// isLive reports whether the host holds a process for one plugin. A
// plugin whose source gave up is not one: its entry stays tracked — the
// loss was reported and what the record says is the application's to
// change — but there is no process behind it, and a caller asking what
// has to come back after a stop (see [Host.changeLocked]) has to be told
// so, or a change restarts a plugin the application was already told is
// gone.
func (h *Host) isLive(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, running := h.live[id]; !running {
		return false
	}
	return !h.lost[id]
}

// SkillRoots returns the absolute skill directories of every enabled
// plugin that declares skills:provide.
//
// The empty list of an unreadable enable state is reported rather than
// passed off as "this host provides no skills": the state file is
// fail-closed everywhere (see [Store.scan]), so an empty answer is what
// the application was shown, and the reason has to be visible in the
// log. The signature carries no error because this method is also the
// `skills` half of the seam core's hooks runner reads (see
// [Host.HookSources]), and a demo whose plugin tree is unreadable is
// not a reason to refuse a runner its sources.
func (h *Host) SkillRoots() []string {
	entries, err := h.store.Enabled()
	if err != nil {
		telemetry.WarnErr(context.Background(),
			"plugin host: cannot list the skills of the enabled plugins", err)
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.Manifest.HasPermission("skills:provide") {
			continue
		}
		for _, rel := range entry.Manifest.Skills {
			if path, err := ResolvePath(entry.Dir, rel); err == nil {
				out = append(out, path)
			} else {
				telemetry.WarnErr(context.Background(),
					"plugin host: skill directory is not usable", err,
					attribute.String("plugin", entry.ID))
			}
		}
	}
	sort.Strings(out)
	return out
}

// HookSources returns the hook files of every enabled plugin that
// declares hooks:provide, each anchored to its plugin directory and
// marked untrusted. Both facts matter to a runner: a plugin's hooks.json
// expects to run inside its own directory, and it is third-party
// content, so the runner strips the content-bearing payload fields
// before those commands see them. Sources are ordered by path, so hook
// execution order is stable across calls.
//
// Like [Host.SkillRoots], the empty list of an unreadable enable state
// is reported rather than passed off as "no plugin contributes hooks".
//
// The signature is the one core's [hooks.SourceProvider] declares, and
// this method is what makes a craft host that provider, so the error
// cannot be returned from here; the runner reads its sources at every
// build, and a warning per build is what an application sees of a
// plugin tree it cannot read.
func (h *Host) HookSources() []hooks.ExtraSource {
	entries, err := h.store.Enabled()
	if err != nil {
		telemetry.WarnErr(context.Background(),
			"plugin host: cannot list the hooks of the enabled plugins", err)
		return nil
	}
	var out []hooks.ExtraSource
	for _, entry := range entries {
		if !entry.Manifest.HasPermission("hooks:provide") {
			continue
		}
		for _, rel := range entry.Manifest.Hooks {
			if path, err := ResolvePath(entry.Dir, rel); err == nil {
				out = append(out, hooks.ExtraSource{
					Path: path,
					Dir:  entry.Dir,
				})
			} else {
				telemetry.WarnErr(context.Background(),
					"plugin host: hook file is not usable", err,
					attribute.String("plugin", entry.ID))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Close stops every running plugin.
func (h *Host) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	ids := make([]string, 0, len(h.live))
	for id := range h.live {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	// The watch ends before the plugins do: a pass in flight would
	// otherwise be able to start a process behind the close.
	h.stopRevisionWatch()
	sort.Strings(ids)
	var errs []error
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), h.drainTimeout)
		if err := h.stopEntry(ctx, id); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}
	if err := h.tools.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (h *Host) startEntry(ctx context.Context, entry Entry) error {
	// One plugin's start and stop are serialized with each other: the
	// open spawns a process and the record is written after it, and
	// without the lock a disable (or Close) that ran in between would
	// leave a process nobody tracks.
	lock := h.entryLock(entry.ID)
	lock.Lock()
	defer lock.Unlock()
	_, err := h.startEntryLocked(ctx, entry)
	return err
}

// startEntryLocked is startEntry's body; callers hold the plugin's entry
// lock. The returned source is the one this call opened, and nil when the
// plugin was already running: an enable waits on the source it started,
// not on one another call is responsible for.
func (h *Host) startEntryLocked(ctx context.Context, entry Entry) (Source, error) {
	// A plugin whose tool namespace another plugin claims cannot be
	// published, so it is not started: whoever asked (an enable, a
	// change that would restart it, Start replaying the record) is told
	// what the collision is instead of getting a process whose tools do
	// not exist.
	if entry.Conflict != "" {
		return nil, errdefs.Conflictf(
			"plugin host: plugin %q cannot be started: %s", entry.ID, entry.Conflict)
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, errdefs.NotAvailablef("plugin host: closed")
	}
	// A plugin whose source gave up is not a running plugin: the entry
	// still says enabled and its tools are still projected — the loss was
	// reported, and whether the plugin stays enabled is the application's
	// call — but the process is gone with no reconnection coming, so an
	// enable of this plugin replaces the source instead of finding it
	// "already running" and reporting a start that never happened.
	gaveUp := h.lost[entry.ID]
	delete(h.lost, entry.ID)
	if gaveUp {
		delete(h.live, entry.ID)
	} else if _, running := h.live[entry.ID]; running {
		h.mu.Unlock()
		return nil, nil
	}
	// Every process this call starts is a new generation, minted before
	// the environment hook runs: that hook is where the token of this
	// process is issued, and it has to know which process it is issuing
	// for (see [Host.SetPluginHooks]).
	h.nextGeneration++
	generation := h.nextGeneration
	h.mu.Unlock()
	if gaveUp {
		h.releaseGaveUp(ctx, entry.ID)
	}
	source, err := h.openSource(ctx, entry, generation)
	if err != nil {
		// The environment hook runs before the source is opened — it is
		// where a process is minted for — so an open that failed can
		// leave a token for a process that does not exist. The stopped
		// hook is what revokes one, and it takes it back here exactly as
		// it does for a process the host let go of.
		h.runStoppedHook(entry.ID)
		return nil, err
	}
	// The open ran outside the host lock (it spawns a process), so the
	// host may have closed in the meantime. Recording the source first
	// means Close's id snapshot sees it and drains it; a source the
	// snapshot missed is closed here instead of being left running
	// behind a closed host.
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = source.Close()
		// Closing the source is not what revokes the token minted for
		// it: the stopped hook is, and this process is one the host is
		// letting go of (see [Host.runStoppedHook]).
		h.runStoppedHook(entry.ID)
		return nil, errdefs.NotAvailablef("plugin host: closed")
	}
	h.live[entry.ID] = livePlugin{
		entry:      entry,
		generation: generation,
		source:     source,
	}
	h.mu.Unlock()
	if err := h.tools.AddPlugin(entry.ID, source); err != nil {
		h.forgetSource(entry.ID, source)
		_ = source.Close()
		// The process this start opened is one the host is letting go
		// of, and closing the source is not what revokes the token
		// minted for it: the stopped hook is (see
		// [Host.runStoppedHook]).
		h.runStoppedHook(entry.ID)
		return nil, err
	}
	return source, nil
}

// releaseGaveUp drops one plugin's recorded source — a process that gave
// up on its own — and releases it before its replacement is opened: the
// tools it still projected are removed, its resources closed, and the
// internal stopped hook run, so the host token minted for the dead
// process is revoked before the new source mints its own. Nothing is
// reported: the loss was the report, and replacing a source that gave up
// is not a stop the application asked for.
func (h *Host) releaseGaveUp(ctx context.Context, id string) {
	drainCtx, cancel := context.WithTimeout(ctx, h.drainTimeout)
	defer cancel()
	if err := h.tools.RemovePlugin(drainCtx, id); err != nil {
		telemetry.WarnErr(ctx, "plugin host: releasing a plugin that gave up failed",
			err, attribute.String("plugin", id))
	}
	h.runStoppedHook(id)
}

// entryLock returns the mutex that serializes one plugin's start and
// stop.
func (h *Host) entryLock(id string) *sync.Mutex {
	h.mu.Lock()
	defer h.mu.Unlock()
	lock := h.entryLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		h.entryLocks[id] = lock
	}
	return lock
}

// forgetSource drops one entry's record when it is still the source
// that was recorded, so a failed publication cannot hide the process it
// left behind.
func (h *Host) forgetSource(id string, source Source) {
	h.mu.Lock()
	if live, ok := h.live[id]; ok && live.source == source {
		delete(h.live, id)
		delete(h.lost, id)
	}
	h.mu.Unlock()
}

// openSource builds the tool source of one plugin: its socket, and the
// process that dials it. The tools:provide gate is not applied here — a
// plugin without it still runs, because it may contribute skills or hooks,
// and its declarations are dropped where they are projected instead (see
// [wireSource.project]). Direct callers — plugin graph nodes — get the
// refusal as their error rather than an empty tool result.
func (h *Host) openSource(
	ctx context.Context,
	entry Entry,
	generation uint64,
) (Source, error) {
	dataDir, err := h.store.DataDir(entry.ID)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	env := make(map[string]string, len(h.env)+2)
	for key, value := range h.env {
		env[key] = value
	}
	envHook := h.envHook
	h.mu.Unlock()
	if envHook != nil {
		if err := envHook(entry, generation, env); err != nil {
			return nil, err
		}
	}
	source, err := h.newSource(ctx, entry, generation, env, dataDir)
	if err != nil {
		return nil, err
	}
	if source == nil {
		// A factory that reports success and hands back nothing is a
		// bug in the application's seam rather than a plugin that
		// publishes nothing ([staticSource] is that), and every caller
		// below takes the source's Close for granted.
		return nil, errdefs.Internal(fmt.Errorf(
			"plugin host: plugin %s: the source factory returned no source",
			entry.ID))
	}
	return source, nil
}

func (h *Host) stopEntry(ctx context.Context, id string) error {
	// The mirror of startEntry: waiting for the per-entry lock means a
	// disable that raced an enable stops the process that enable
	// spawned, not the state it saw before.
	lock := h.entryLock(id)
	lock.Lock()
	defer lock.Unlock()
	return h.stopEntryLocked(ctx, id, StopRequested)
}

// stopEntryLocked is stopEntry's body; callers hold the plugin's entry
// lock. reason is what the stop report carries: the same stop looks
// different to an application depending on whether it asked for it.
func (h *Host) stopEntryLocked(
	ctx context.Context,
	id string,
	reason StopReason,
) error {
	h.mu.Lock()
	live, running := h.live[id]
	delete(h.live, id)
	delete(h.lost, id)
	// The stop is narrated unless this process is already narrating its
	// own loss: the loss and the stop of one process are one event, and
	// the application hears it once (see [Host.notifyLost]).
	narrate := running && h.claimStopReportLocked(id, live.generation)
	h.mu.Unlock()
	if !running {
		return nil
	}
	drainCtx, cancel := context.WithTimeout(ctx, h.drainTimeout)
	defer cancel()
	err := h.tools.RemovePlugin(drainCtx, id)
	if narrate {
		h.notifyStopped(id, reason)
		h.mu.Lock()
		h.releaseStopReportLocked(id, live.generation)
		h.mu.Unlock()
		return err
	}
	// The half of the stop that is not an application-facing event runs
	// either way: the host is letting this process go, so the token
	// minted for it is revoked whether or not this stop is the report
	// the application hears.
	h.runStoppedHook(id)
	return err
}
