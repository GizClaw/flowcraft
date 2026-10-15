package plugin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"
)

// readyFactory is a scripted NewSourceFunc whose sources implement
// [Readiness]: every plugin it is asked for gets a WaitReady that
// records the bound the host handed it and answers with the test's
// function.
type readyFactory struct {
	wait func(ctx context.Context, timeout time.Duration) error

	mu     sync.Mutex
	calls  []time.Duration
	closed map[string]int
}

func newReadyFactory(wait func(ctx context.Context, timeout time.Duration) error) *readyFactory {
	return &readyFactory{wait: wait, closed: map[string]int{}}
}

func (f *readyFactory) new(
	_ context.Context,
	entry Entry,
	_ map[string]string,
	_ string,
) (Source, error) {
	source := &fakeSource{tools: []tool.Tool{fakeTool{name: entry.ID + "__echo"}}}
	source.close = func() {
		f.mu.Lock()
		f.closed[entry.ID]++
		f.mu.Unlock()
	}
	return &readySource{fakeSource: source, factory: f}, nil
}

// readySource is one fake source that can report readiness.
type readySource struct {
	*fakeSource
	factory *readyFactory
}

func (s *readySource) WaitReady(ctx context.Context, timeout time.Duration) error {
	f := s.factory
	f.mu.Lock()
	f.calls = append(f.calls, timeout)
	wait := f.wait
	f.mu.Unlock()
	if wait == nil {
		return nil
	}
	return wait(ctx, timeout)
}

// answerWait scripts what the next readiness wait says. It is set after
// the host exists, so a wait that reports a loss can reach it.
func (f *readyFactory) answerWait(wait func(ctx context.Context, timeout time.Duration) error) {
	f.mu.Lock()
	f.wait = wait
	f.mu.Unlock()
}

func (f *readyFactory) waits() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.calls...)
}

func (f *readyFactory) closes(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[id]
}

// hookRecorder records the plugin ids craft's own stopped hook was told
// about, the seam that revokes a plugin's host tokens.
type hookRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (h *hookRecorder) record(id string) {
	h.mu.Lock()
	h.ids = append(h.ids, id)
	h.mu.Unlock()
}

func (h *hookRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ids)
}

// enabled reports one plugin's recorded enable state.
func enabled(t *testing.T, store *Store, id string) bool {
	t.Helper()
	entry, ok := store.Entry(id)
	if !ok {
		t.Fatalf("plugin %s is not in the store", id)
	}
	return entry.Enabled
}

// toolNames lists the published tool names.
func toolNames(host *Host) []string {
	names := []string{}
	for _, candidate := range host.ToolSet().Tools() {
		names = append(names, candidate.Definition().Name)
	}
	return names
}

// running reports whether the host still records a source for one
// plugin.
func running(host *Host, id string) bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	_, ok := host.live[id]
	return ok
}

// startReadyHost starts a host over a fresh store holding one disabled
// plugin, with both stop seams recorded. A plugin is enabled by default
// on a fresh state file, so the store is told to disable it first: the
// tests here are about what enabling does.
func startReadyHost(
	t *testing.T,
	opts HostOptions,
	factory *readyFactory,
) (*Host, *Store, *stopEvents, *hookRecorder) {
	t.Helper()
	root := t.TempDir()
	writePlugin(t, root, "hello", contributionManifest("hello", "mcp:provide"))
	writeContributionTree(t, root, "hello")
	store := newStoreOver(t, root)
	if err := store.SetEnabled("hello", false); err != nil {
		t.Fatalf("disable before start: %v", err)
	}
	opts.Store = store
	opts.NewSource = factory.new
	host, err := NewHost(opts)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	events := &stopEvents{}
	hook := &hookRecorder{}
	host.SetPluginHooks(nil, hook.record)
	host.OnStopped(events.record)
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host, store, events, hook
}

// TestHostEnableWaitsForReady covers the wait itself: an enable asks the
// source it started whether the plugin is up, with the host's bound, and
// records the entry once the answer is yes. Enabling a plugin that is
// already running starts nothing, so it waits for nothing.
func TestHostEnableWaitsForReady(t *testing.T) {
	t.Parallel()
	factory := newReadyFactory(nil)
	bound := 5 * time.Second
	host, store, events, _ := startReadyHost(t, HostOptions{EnableTimeout: bound}, factory)
	ctx := context.Background()

	if err := host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := factory.waits(); !slices.Equal(got, []time.Duration{bound}) {
		t.Fatalf("readiness waits = %v, want one with the host's bound", got)
	}
	if !enabled(t, store, "hello") {
		t.Fatal("the entry was not recorded enabled")
	}
	if !running(host, "hello") {
		t.Fatal("the source is not recorded")
	}
	if got := toolNames(host); !slices.Contains(got, "hello__echo") {
		t.Fatalf("tools = %v, want the plugin's", got)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("an enable that succeeded reported %v", got)
	}

	if err := host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("enable again: %v", err)
	}
	if got := factory.waits(); len(got) != 1 {
		t.Fatalf("readiness waits = %v, want no wait for an enable that started nothing", got)
	}
}

// TestHostEnableRollsBackUnrunnablePlugin covers the terminal half: the
// plugin cannot come up at all, so the enable is undone — the process is
// stopped, the record is left disabled, the tokens are revoked, and the
// stop is reported with the reason an application did not ask for.
func TestHostEnableRollsBackUnrunnablePlugin(t *testing.T) {
	t.Parallel()
	factory := newReadyFactory(func(context.Context, time.Duration) error {
		return errdefs.Validationf("plugin hello: mcp: server hello: " +
			"cannot start command: fork/exec ./server: permission denied")
	})
	host, store, events, hook := startReadyHost(t, HostOptions{}, factory)
	ctx := context.Background()

	err := host.SetEnabled(ctx, "hello", true)
	if !errdefs.IsValidation(err) {
		t.Fatalf("enable = %v, want the source's terminal failure", err)
	}
	if !strings.Contains(err.Error(), "cannot start command") {
		t.Fatalf("enable error = %v, want the cause the source reported", err)
	}
	if enabled(t, store, "hello") {
		t.Fatal("a rolled-back enable was recorded as enabled")
	}
	if running(host, "hello") {
		t.Fatal("the rolled-back source is still recorded")
	}
	if got := factory.closes("hello"); got != 1 {
		t.Fatalf("the rolled-back process was closed %d times, want 1", got)
	}
	if got := toolNames(host); len(got) != 0 {
		t.Fatalf("tools = %v, want none", got)
	}
	if got := events.snapshot(); !slices.Equal(got, []string{"hello:rolled_back"}) {
		t.Fatalf("stop events = %v, want [hello:rolled_back]", got)
	}
	if got := hook.count(); got != 1 {
		t.Fatalf("the internal stopped hook ran %d times, want 1", got)
	}

	// Nothing was recorded, so a second enable is a second attempt
	// rather than a no-op that reports success.
	if err := host.SetEnabled(ctx, "hello", true); !errdefs.IsValidation(err) {
		t.Fatalf("second enable = %v, want the same terminal failure", err)
	}
}

// TestHostEnableKeepsTryingUnreadyPlugin covers the transient half: a
// server that is not there yet is not a failure of enable, so the plugin
// is recorded enabled with its source left to reconnect, and nothing is
// reported as stopped.
func TestHostEnableKeepsTryingUnreadyPlugin(t *testing.T) {
	t.Parallel()
	factory := newReadyFactory(func(context.Context, time.Duration) error {
		return errdefs.NotAvailablef("mcp: server %q: unreachable: connection refused", "hello")
	})
	host, store, events, hook := startReadyHost(t, HostOptions{}, factory)

	if err := host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable of a plugin that is not up yet: %v", err)
	}
	if !enabled(t, store, "hello") {
		t.Fatal("the entry was not recorded enabled")
	}
	if !running(host, "hello") {
		t.Fatal("the source was dropped instead of left reconnecting")
	}
	if got := factory.closes("hello"); got != 0 {
		t.Fatalf("a reconnecting source was stopped %d times", got)
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("a plugin that is not up yet was reported as stopped: %v", got)
	}
	if got := hook.count(); got != 0 {
		t.Fatalf("the internal stopped hook ran for a plugin that is still trying: %v", got)
	}
}

// TestHostEnableBoundsTheWait pins the bound: a source that is still
// connecting when the timeout elapses does not hold the enable open, and
// does not turn into a rollback either — waiting can tell "not yet" from
// "never", and only the second one undoes an enable.
func TestHostEnableBoundsTheWait(t *testing.T) {
	t.Parallel()
	bound := 50 * time.Millisecond
	factory := newReadyFactory(func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	})
	host, store, events, _ := startReadyHost(t, HostOptions{EnableTimeout: bound}, factory)

	start := time.Now()
	if err := host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if elapsed := time.Since(start); elapsed < bound || elapsed > 5*time.Second {
		t.Fatalf("enable took %s, want it bounded by %s", elapsed, bound)
	}
	if !enabled(t, store, "hello") {
		t.Fatal("a source that ran out of time was rolled back")
	}
	if !running(host, "hello") {
		t.Fatal("the source was dropped when the bound elapsed")
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("a plugin that is still connecting was reported as stopped: %v", got)
	}
}

// TestHostEnableWithoutWait pins the opt-out: a host configured with a
// negative timeout never asks a source whether it is up, which is what
// every host did before the wait existed.
func TestHostEnableWithoutWait(t *testing.T) {
	t.Parallel()
	factory := newReadyFactory(func(context.Context, time.Duration) error {
		return errdefs.Validationf("no host configured without the wait may get here")
	})
	host, store, _, _ := startReadyHost(t, HostOptions{EnableTimeout: -1}, factory)

	if err := host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := factory.waits(); len(got) != 0 {
		t.Fatalf("a host that does not wait asked %v", got)
	}
	if !enabled(t, store, "hello") {
		t.Fatal("the entry was not recorded enabled")
	}
	if !running(host, "hello") {
		t.Fatal("the source is not recorded")
	}
}

// TestHostStartDoesNotWait pins the asymmetry: startup replays the enable
// state a previous session recorded, so it neither blocks on a plugin's
// process nor undoes the state it replayed. A plugin that cannot come up
// reaches the application through [Host.OnStopped] instead.
func TestHostStartDoesNotWait(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	factory := newReadyFactory(func(ctx context.Context, _ time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})
	root := t.TempDir()
	writePlugin(t, root, "hello", contributionManifest("hello", "mcp:provide"))
	writeContributionTree(t, root, "hello")
	// A fresh state file enables a scanned plugin, so this is the startup
	// path: the plugin is enabled before Start reads it.
	store := newStoreOver(t, root)
	host, err := NewHost(HostOptions{
		Store:         store,
		NewSource:     factory.new,
		EnableTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })

	start := time.Now()
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Start took %s: it waited for the plugin", elapsed)
	}
	if got := factory.waits(); len(got) != 0 {
		t.Fatalf("Start asked for readiness %v", got)
	}
	if !enabled(t, store, "hello") {
		t.Fatal("Start changed the state it replayed")
	}
}

// TestHostEnableWaitDoesNotHoldUpClose pins the interplay with
// shutdown: a Close that races an in-flight wait waits for the plugin's
// entry lock — bounded by the same timeout — and then stops what the
// enable started instead of leaving a process behind a closed host.
func TestHostEnableWaitDoesNotHoldUpClose(t *testing.T) {
	t.Parallel()
	var host *Host
	var events *stopEvents
	factory := newReadyFactory(func(ctx context.Context, _ time.Duration) error {
		// A loss that races the close is not reported either: the host
		// is closing, and the stop below is what the application hears
		// about this plugin.
		host.notifyLost("hello")
		<-ctx.Done()
		return ctx.Err()
	})
	host, _, events, _ = startReadyHost(
		t, HostOptions{EnableTimeout: 200 * time.Millisecond}, factory)
	// The helper registers its own Close; this test closes the host
	// first, and Close is idempotent.
	enableDone := make(chan error, 1)
	go func() {
		enableDone <- host.SetEnabled(context.Background(), "hello", true)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(factory.waits()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the enable never reached its wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- host.Close() }()

	select {
	case err := <-enableDone:
		if err != nil {
			t.Fatalf("enable: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Close deadlocked the enable")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the in-flight enable held Close up")
	}
	if running(host, "hello") {
		t.Fatal("a closed host still records the plugin it stopped")
	}
	if got := events.snapshot(); !slices.Equal(got, []string{"hello:requested"}) {
		t.Fatalf("stop events = %v, want [hello:requested]", got)
	}
}

// realPlugin is one scanned plugin plus the host serving it, with the
// plugin disabled.
type realPlugin struct {
	host  *Host
	store *Store
	// dataRoot is where the plugin's data directory lives, which is where
	// a helper process records its pid.
	dataRoot string
}

// realPluginHost starts a host over a store holding the one plugin the
// caller wrote into root, disabled, so that enabling it is the call
// under test.
func realPluginHost(t *testing.T, root string, opts HostOptions) *realPlugin {
	t.Helper()
	dataRoot := t.TempDir()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: dataRoot,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.SetEnabled("hello", false); err != nil {
		t.Fatalf("disable before start: %v", err)
	}
	opts.Store = store
	host, err := NewHost(opts)
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return &realPlugin{host: host, store: store, dataRoot: dataRoot}
}

// helperManifest is a manifest whose stdio server is this test binary
// re-executed as the stop helper: one echo tool, and a command that is
// there and executable.
func helperManifest() string {
	return `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"],
		"mcp": {
			"command": "./server",
			"args": ["-test.run=TestPluginStopHelper"],
			"env": {"CRAFT_PLUGIN_STOP_HELPER": "1"}
		}
	}`
}

// writeHelperCommand links this test binary into the plugin directory as
// its server.
func writeHelperCommand(t *testing.T, dir string) {
	t.Helper()
	if os.Getenv("CRAFT_PLUGIN_STOP_HELPER") == "1" {
		t.Skip("running as helper")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "server")); err != nil {
		t.Fatalf("symlink helper: %v", err)
	}
}

// TestHostEnableWaitsForRealPlugin covers the promise end to end: the
// enable returns once the plugin's tools are published, not once its
// process has been spawned, so a caller that gets nil can call them.
func TestHostEnableWaitsForRealPlugin(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", helperManifest())
	writeHelperCommand(t, filepath.Join(root, "hello"))
	plugin := realPluginHost(t, root, HostOptions{})

	if err := plugin.host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := toolNames(plugin.host); !slices.Contains(got, "hello__echo") {
		t.Fatalf("enable returned before the plugin was up: tools = %v", got)
	}
	if !enabled(t, plugin.store, "hello") {
		t.Fatal("the entry was not recorded enabled")
	}
}

// TestHostEnableKeepsTryingUnreachableServer covers the real retrying
// class end to end: a remote server that is down is not a failure of
// enable, so the plugin is recorded enabled with its source still
// reconnecting, and the application is not told the plugin stopped.
func TestHostEnableKeepsTryingUnreachableServer(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"],
		"mcp": {"transport": "http", "url": "http://127.0.0.1:1/mcp"}
	}`)
	bound := 200 * time.Millisecond
	plugin := realPluginHost(t, root, HostOptions{EnableTimeout: bound})
	events := &stopEvents{}
	plugin.host.OnStopped(events.record)

	start := time.Now()
	if err := plugin.host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable of an unreachable server: %v", err)
	}
	if elapsed := time.Since(start); elapsed < bound {
		t.Fatalf("enable returned in %s, before the bound it was given", elapsed)
	}
	if !enabled(t, plugin.store, "hello") {
		t.Fatal("the entry was not recorded enabled")
	}
	if !running(plugin.host, "hello") {
		t.Fatal("the source was dropped instead of left reconnecting")
	}
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("an unreachable server was reported as stopped: %v", got)
	}
}

// TestHostEnableRollbackReportsARecordItCouldNotWrite covers the half of
// a rollback that is not the process. The stop is what the caller asked
// for; the record that says enabled about a plugin with no process goes
// with it, and a write that fails is reported alongside the plugin's own
// failure rather than dropped. Nothing else repairs that disagreement —
// the failed write bumps no revision, so no reconcile pass visits a
// plugin that is no longer live — and the caller is the one that can
// still act on it.
func TestHostEnableRollbackReportsARecordItCouldNotWrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writePlugin(t, root, "hello", contributionManifest("hello", "mcp:provide"))
	writeContributionTree(t, root, "hello")
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    stateDir,
		DataDirRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	factory := newReadyFactory(func(context.Context, time.Duration) error {
		return errdefs.Validationf(
			"plugin hello: mcp: server hello: cannot start command: " +
				"fork/exec ./server: no such file or directory")
	})
	host, err := NewHost(HostOptions{Store: store, NewSource: factory.new})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	// The plugin is enabled by default — no record was ever written — and
	// the rollback's write is the one that cannot land.
	if !enabled(t, store, "hello") {
		t.Fatal("the fixture is not the default-enabled one this test needs")
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	err = host.SetEnabled(context.Background(), "hello", true)
	if err == nil {
		t.Fatal("enable of a plugin that cannot come up reported success")
	}
	if !strings.Contains(err.Error(), "cannot start command") {
		t.Fatalf("enable error = %v, want the source's terminal failure", err)
	}
	if !strings.Contains(err.Error(), "recording it disabled") {
		t.Fatalf("enable error = %v, want the record write it could not make", err)
	}
	if running(host, "hello") {
		t.Fatal("the rolled-back enable left a process")
	}
	// The record is the thing that could not be written: it still says
	// enabled, which is exactly what the caller was told about.
	if !enabled(t, store, "hello") {
		t.Fatal("the record changed although its write failed")
	}
}
