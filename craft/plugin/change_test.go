package plugin

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// versionFactory is a scripted NewSourceFunc for the change tests: it
// records which plugin version each process was started from and
// whether it came up, so a test can watch which version is running and
// make the next one unable to start.
type versionFactory struct {
	mu      sync.Mutex
	started []string
	closes  []string
	refuse  map[string]bool
	// block, when non-nil, is what every tool call of every source this
	// factory builds waits for; startedCall is closed when the first
	// call is in flight. Together they let a test hold a call open, so a
	// stop that waits for it runs into its drain bound.
	block       chan struct{}
	startedCall chan struct{}
	// startedOnce guards the close of startedCall against a second call
	// arriving before the test looked at it.
	startedOnce sync.Once
}

func newVersionFactory() *versionFactory {
	return &versionFactory{refuse: map[string]bool{}}
}

func (f *versionFactory) new(
	_ context.Context,
	entry Entry,
	_ map[string]string,
	_ string,
) (Source, error) {
	version := entry.Manifest.Version
	f.mu.Lock()
	f.started = append(f.started, version)
	f.mu.Unlock()
	source := &fakeSource{tools: []tool.Tool{fakeTool{name: entry.ID + "__echo"}}}
	source.close = func() {
		f.mu.Lock()
		f.closes = append(f.closes, version)
		f.mu.Unlock()
	}
	if f.block != nil {
		// A published tool that never finishes is what a stop's drain
		// waits for; the plain fake tool executes immediately.
		source.tools = []tool.Tool{blockingTool{factory: f, name: entry.ID + "__echo"}}
	}
	return &versionSource{fakeSource: source, factory: f, version: version}, nil
}

// signalCallStarted closes the test's channel once, when a call is in
// flight. The field is written before the host starts, so reading it
// here (from the call's own goroutine) is ordered by the goroutine that
// started the call.
func (f *versionFactory) signalCallStarted() {
	f.startedOnce.Do(func() {
		if f.startedCall != nil {
			close(f.startedCall)
		}
	})
}

// refuseVersion makes every process started from that version unable to
// come up: the spawn succeeds, the server does not.
func (f *versionFactory) refuseVersion(version string) {
	f.mu.Lock()
	f.refuse[version] = true
	f.mu.Unlock()
}

func (f *versionFactory) refusing(version string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refuse[version]
}

func (f *versionFactory) versions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

func (f *versionFactory) closedVersions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.closes...)
}

// versionSource reports readiness with the version it was built from, so
// a refused version is a terminal failure of that process.
type versionSource struct {
	*fakeSource
	factory *versionFactory
	version string
}

func (s *versionSource) WaitReady(context.Context, time.Duration) error {
	if s.factory.refusing(s.version) {
		return errdefs.Validationf(
			"plugin hello: mcp: server hello: cannot start command: " +
				"fork/exec ./server: no such file or directory")
	}
	return nil
}

// startVersionHost starts a host over one root holding the versions a
// test wrote, with the factory recorded.
func startVersionHost(t *testing.T, factory *versionFactory) (*Host, *Store) {
	t.Helper()
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	store := newStoreOver(t, root)
	host := startHost(t, store, factory.new)
	return host, store
}

// installVersion writes one version of the plugin next to the root and
// installs it through the host.
func installVersion(
	t *testing.T,
	host *Host,
	version string,
) (Summary, error) {
	t.Helper()
	source := t.TempDir()
	writePlugin(t, source, "hello", manifestFor("hello", version))
	return host.Install(context.Background(), filepath.Join(source, "hello"))
}

// TestHostInstallRestartsTheRunningVersion covers the half of an install
// that is not the store's: the directory a running plugin's process
// reads is replaced, so the process is replaced too. What the record
// says is installed and what the process runs are the same version.
func TestHostInstallRestartsTheRunningVersion(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	host, store := startVersionHost(t, factory)
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0"}) {
		t.Fatalf("started versions = %v, want the installed one", got)
	}

	summary, err := installVersion(t, host, "0.2.0")
	if err != nil || summary.Version != "0.2.0" {
		t.Fatalf("Install = %+v, %v", summary, err)
	}
	// The old process is gone and the new one was started from the new
	// bytes: an install does not leave the old code serving.
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0", "0.2.0"}) {
		t.Fatalf("started versions = %v, want the old and then the new", got)
	}
	if got := factory.closedVersions(); !slices.Equal(got, []string{"0.1.0"}) {
		t.Fatalf("closed versions = %v, want the replaced process", got)
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Manifest.Version != "0.2.0" {
		t.Fatalf("entry after the install = %+v, %v", entry, ok)
	}
	if !entry.Enabled {
		t.Fatal("a running plugin was left recorded disabled by its own update")
	}
	if !running(host, "hello") {
		t.Fatal("the host has no process after the install")
	}
	if names := toolNames(host); !slices.Contains(names, "hello__echo") {
		t.Fatalf("tools after the install = %v, want the plugin's", names)
	}
}

// TestHostChangeUndoesAVersionThatCannotRun covers the other half: the
// version that was installed cannot come up, so the version that ran
// before is put back and started, and the caller hears why the new one
// failed. The one outcome that is not allowed is a record that says
// enabled about a plugin whose process is not the recorded version.
func TestHostChangeUndoesAVersionThatCannotRun(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	factory.refuseVersion("0.2.0")
	host, store := startVersionHost(t, factory)

	_, err := installVersion(t, host, "0.2.0")
	if !errdefs.IsValidation(err) {
		t.Fatalf("Install of an unrunnable version = %v, want the source's terminal failure", err)
	}
	if !strings.Contains(err.Error(), "cannot start command") {
		t.Fatalf("Install error = %v, want the cause the source reported", err)
	}
	// The installed version is the one that runs again.
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0", "0.2.0", "0.1.0"}) {
		t.Fatalf("started versions = %v, want the old, the new, and the old again", got)
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Manifest.Version != "0.1.0" {
		t.Fatalf("entry after the undo = %+v, %v, want the previous version back", entry, ok)
	}
	if !entry.Enabled {
		t.Fatal("the plugin was left recorded disabled after the version was put back")
	}
	if !running(host, "hello") {
		t.Fatal("the previous version is not running again")
	}
	if names := toolNames(host); !slices.Contains(names, "hello__echo") {
		t.Fatalf("tools after the undo = %v, want the plugin's", names)
	}
}

// TestHostChangeDoesNotStartAStoppedPlugin pins the boundary the change
// protocol works inside: an install updates what is installed, and a
// plugin that was not running is not switched on by it.
func TestHostChangeDoesNotStartAStoppedPlugin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	store := newStoreOver(t, root)
	if err := store.SetEnabled("hello", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	factory := newVersionFactory()
	host := startHost(t, store, factory.new)

	summary, err := installVersion(t, host, "0.2.0")
	if err != nil || summary.Version != "0.2.0" {
		t.Fatalf("Install = %+v, %v", summary, err)
	}
	if got := factory.versions(); len(got) != 0 {
		t.Fatalf("started versions = %v, want nothing started for a stopped plugin", got)
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Manifest.Version != "0.2.0" || entry.Enabled {
		t.Fatalf("entry after the install = %+v, %v", entry, ok)
	}
}

// TestHostNumbersEveryProcessItStarts pins what the environment hook is
// told: every process the host starts gets a generation of its own, and
// the generation of a replaced process is never handed out again. That
// is what lets a caller mint per process and revoke per process — the
// grants a token carries belong to one process's manifest.
func TestHostNumbersEveryProcessItStarts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	store := newStoreOver(t, root)
	factory := newVersionFactory()
	host, err := NewHost(HostOptions{Store: store, NewSource: factory.new})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	var mu sync.Mutex
	generations := []uint64{}
	host.SetPluginHooks(func(
		_ Entry,
		generation uint64,
		_ map[string]string,
	) error {
		mu.Lock()
		generations = append(generations, generation)
		mu.Unlock()
		return nil
	}, nil)
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	ctx := context.Background()
	if err := host.SetEnabled(ctx, "hello", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(generations) != 2 || generations[0] == generations[1] {
		t.Fatalf("generations = %v, want two distinct ones, one per process",
			generations)
	}
	if generations[1] <= generations[0] {
		t.Fatalf("generations = %v, want the later process numbered higher",
			generations)
	}
}

// TestHostInstallOfANewPluginLeavesItRecordedDisabled pins the record a
// fresh install leaves behind. A store with no record for an id reads it
// enabled, and an install does not turn a plugin on — so a fresh install
// that wrote no record would leave an entry saying enabled about a
// plugin with no process behind it, the one state a running host does
// not leave behind. Enabling it is what starts it.
func TestHostInstallOfANewPluginLeavesItRecordedDisabled(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	root := t.TempDir()
	store := newStoreOver(t, root)
	host := startHost(t, store, factory.new)

	summary, err := installVersion(t, host, "0.1.0")
	if err != nil || summary.Version != "0.1.0" {
		t.Fatalf("Install = %+v, %v", summary, err)
	}
	if got := factory.versions(); len(got) != 0 {
		t.Fatalf("started versions = %v, want nothing started for a fresh install", got)
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Enabled {
		t.Fatalf("entry after the install = %+v, %v, want it recorded disabled",
			entry, ok)
	}
	if running(host, "hello") {
		t.Fatal("the host has a process for a plugin it was not asked to start")
	}
	if names := toolNames(host); len(names) != 0 {
		t.Fatalf("tools after the install = %v, want none", names)
	}

	// The enable is the answer that starts it, from the install that is
	// already on disk.
	if err := host.SetEnabled(context.Background(), "hello", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0"}) {
		t.Fatalf("started versions = %v, want the installed one after the enable", got)
	}
	if !running(host, "hello") {
		t.Fatal("the enable left no process")
	}
}

// TestHostUpdateFromRefusesBeforeStopping pins the resolve half of the
// protocol for the update path: what an update would be refused for
// without reading anything — an id that is not installed — is answered
// with the plugin's process untouched.
func TestHostUpdateFromRefusesBeforeStopping(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	host, _ := startVersionHost(t, factory)

	_, err := host.UpdateFrom(context.Background(), "missing", nil)
	if !errdefs.IsNotFound(err) {
		t.Fatalf("UpdateFrom of an unknown plugin = %v, want NotFound", err)
	}
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0"}) {
		t.Fatalf("started versions = %v, want the running plugin untouched", got)
	}
	if got := factory.closedVersions(); len(got) != 0 {
		t.Fatalf("closed versions = %v, want nothing stopped", got)
	}
	if !running(host, "hello") {
		t.Fatal("the refusal stopped the plugin's process")
	}
}

// TestHostRollbackRestoresAndRestarts covers the version rollback the
// store already had, now with the process in step with it: the snapshot
// becomes what is installed and what runs.
func TestHostRollbackRestoresAndRestarts(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	host, store := startVersionHost(t, factory)
	if _, err := installVersion(t, host, "0.2.0"); err != nil {
		t.Fatalf("install 0.2.0: %v", err)
	}

	summary, err := host.Rollback(context.Background(), "hello")
	if err != nil || summary.Version != "0.1.0" {
		t.Fatalf("Rollback = %+v, %v", summary, err)
	}
	if got := factory.versions(); !slices.Equal(got, []string{"0.1.0", "0.2.0", "0.1.0"}) {
		t.Fatalf("started versions = %v, want the restored version serving again", got)
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Manifest.Version != "0.1.0" {
		t.Fatalf("entry after the rollback = %+v, %v", entry, ok)
	}
	if !running(host, "hello") {
		t.Fatal("the restored version is not running")
	}
}

// TestHostChangeLeavesNoEnabledPluginWithoutAProcess covers the last
// resort: neither the version that was installed nor the one that was
// put back can run. A record that says enabled about a plugin with no
// process is the one state no reading of the entry could correct, so the
// record goes — the caller's error is what tells the application why.
func TestHostChangeLeavesNoEnabledPluginWithoutAProcess(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	host, store := startVersionHost(t, factory)
	// The version that is installed now cannot be started any more: the
	// case is a plugin whose program was removed from under it.
	factory.refuseVersion("0.1.0")
	factory.refuseVersion("0.2.0")

	_, err := installVersion(t, host, "0.2.0")
	if err == nil {
		t.Fatal("Install of a version that cannot run reported success")
	}
	if entry, ok := store.Entry("hello"); !ok || entry.Enabled {
		t.Fatalf("entry after the failure = %+v, %v, want it recorded disabled",
			entry, ok)
	}
	if running(host, "hello") {
		t.Fatal("a host is still recorded for a plugin nothing can run")
	}
	if names := toolNames(host); len(names) != 0 {
		t.Fatalf("tools after the failure = %v, want none", names)
	}
}

// TestHostChangeTakesTheProcessOutOfTheWayFirst pins the order of the
// protocol with a stop that cannot finish: a call in flight longer than
// the host's drain bound. The change must not go ahead — the store's
// swap removes the directory the process is still reading — the
// installed version stays what it was, and the record stops claiming a
// process that the stop closed.
func TestHostChangeTakesTheProcessOutOfTheWayFirst(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	factory.block = make(chan struct{})
	started := make(chan struct{})
	factory.startedCall = started
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	store := newStoreOver(t, root)
	host := startHost(t, store, factory.new)
	host.drainTimeout = 20 * time.Millisecond

	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = callToolThrough(context.Background(), host, "hello__echo")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never started")
	}

	_, err := installVersion(t, host, "0.2.0")
	if err == nil {
		t.Fatal("a change went ahead over a plugin whose stop timed out")
	}
	if entry, ok := store.Entry("hello"); !ok || entry.Manifest.Version != "0.1.0" {
		t.Fatalf("entry after the refused change = %+v, %v, want the version untouched",
			entry, ok)
	}
	// The source was closed by the failed stop, so the record must not
	// keep saying enabled about it.
	if entry, _ := store.Entry("hello"); entry.Enabled {
		t.Fatal("a plugin whose process is gone was left recorded enabled")
	}
	if running(host, "hello") {
		t.Fatal("the host still records a process the stop released")
	}
	close(factory.block)
	<-callDone
}

// callToolThrough runs one plugin tool the way a runtime does: through
// the aggregator, which is what a stop waits for.
func callToolThrough(
	ctx context.Context,
	host *Host,
	name string,
) (any, error) {
	for _, candidate := range host.ToolSet().Tools() {
		if candidate.Definition().Name != name {
			continue
		}
		return candidate.Execute(ctx, "{}")
	}
	return nil, errdefs.NotFoundf("tool %q is not published", name)
}

// blockingTool is one plugin tool whose execution waits for the test, so
// a stop that drains in-flight calls has something to wait for.
type blockingTool struct {
	factory *versionFactory
	name    string
}

func (t blockingTool) Definition() message.ToolDefinition {
	return message.ToolDefinition{Name: t.name}
}

func (t blockingTool) Execute(context.Context, string) (message.Content, error) {
	t.factory.signalCallStarted()
	<-t.factory.block
	return message.Content{}, errors.New("call released")
}
