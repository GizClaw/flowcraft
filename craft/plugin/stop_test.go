package plugin

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stopEvents records OnStopped notifications as "id:reason".
type stopEvents struct {
	mu     sync.Mutex
	events []string
}

func (s *stopEvents) record(id string, reason StopReason) {
	s.mu.Lock()
	s.events = append(s.events, id+":"+string(reason))
	s.mu.Unlock()
}

func (s *stopEvents) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// TestHostOnStoppedReportsRequestedStops covers the application-driven
// half of the stop surface: disable, uninstall and Close each report
// StopRequested once; stopping something that is not running reports
// nothing; an unsubscribed callback stops hearing about stops; and
// craft's own stopped hook keeps firing regardless of what the
// application subscribed to.
func TestHostOnStoppedReportsRequestedStops(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, id := range []string{"hello", "quiet", "closing"} {
		writePlugin(t, root, id, contributionManifest(id, "tools.provide"))
		writeContributionTree(t, root, id)
	}
	store := newStoreOver(t, root)
	host, err := NewHost(HostOptions{Store: store, NewSource: newSourceFactory().new})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	ctx := context.Background()
	if err := host.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })

	events := &stopEvents{}
	host.OnStopped(events.record)
	unsubscribed := &stopEvents{}
	unsubscribe := host.OnStopped(unsubscribed.record)
	unsubscribe()
	unsubscribe() // the returned function is idempotent

	var mu sync.Mutex
	var internal []string
	host.SetPluginHooks(nil, func(id string) {
		mu.Lock()
		internal = append(internal, id)
		mu.Unlock()
	})

	if err := host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("enable hello: %v", err)
	}
	if err := host.SetEnabled(ctx, "hello", false); err != nil {
		t.Fatalf("disable hello: %v", err)
	}
	// Stopping a plugin that is not running is not a stop.
	if err := host.SetEnabled(ctx, "hello", false); err != nil {
		t.Fatalf("disable hello twice: %v", err)
	}

	if err := host.SetEnabled(ctx, "quiet", true); err != nil {
		t.Fatalf("enable quiet: %v", err)
	}
	if err := host.Uninstall(ctx, "quiet", UninstallOptions{}); err != nil {
		t.Fatalf("uninstall quiet: %v", err)
	}

	if err := host.SetEnabled(ctx, "closing", true); err != nil {
		t.Fatalf("enable closing: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := []string{
		"hello:" + string(StopRequested),
		"quiet:" + string(StopRequested),
		"closing:" + string(StopRequested),
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("stop events = %v, want %v", got, want)
	}
	if got := unsubscribed.snapshot(); len(got) != 0 {
		t.Fatalf("the unsubscribed callback heard %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(internal) != 3 {
		t.Fatalf("the internal stopped hook fired %d times, want 3 (%v)",
			len(internal), internal)
	}
}

// stopTestPlugin is one running plugin whose process is this test binary
// re-executed, plus the paths the loss tests need.
type stopTestPlugin struct {
	host     *Host
	dir      string
	dataRoot string
}

// startStopPluginHost scans one plugin, starts the host and returns it
// once the plugin's tools are published. A positive exitMS makes the
// helper exit on its own that many milliseconds after it starts.
func startStopPluginHost(t *testing.T, exitMS int) *stopTestPlugin {
	t.Helper()
	if os.Getenv(helperEnv) == "1" {
		t.Skip("running as helper")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "hello")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	env := map[string]string{}
	if exitMS > 0 {
		env[helperExitMS] = strconv.Itoa(exitMS)
	}
	manifest := wireManifestEnv("hello", env, "tools.provide")
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	writeHelperCommand(t, dir)
	dataRoot := t.TempDir()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: dataRoot,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := NewHost(HostOptions{Store: store, Server: newWireServer(t)})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	waitForTool(t, host, "hello__echo")
	return &stopTestPlugin{host: host, dir: dir, dataRoot: dataRoot}
}

// helperPIDs returns the pids the running children recorded, in spawn
// order.
func (p *stopTestPlugin) helperPIDs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.dataRoot, "hello", "pids"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read pid file: %v", err)
	}
	return strings.Fields(string(raw))
}

// waitForPIDs blocks until the plugin spawned at least want children.
func (p *stopTestPlugin) waitForPIDs(t *testing.T, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		pids := p.helperPIDs(t)
		if len(pids) >= want {
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper pids = %v, want at least %d", pids, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestHostOnStoppedReportsTerminalProcessLoss covers the process half of
// the stop surface: a running plugin whose process dies and whose
// reconnect cannot spawn the command again is reported once, with a
// reason distinct from an application-requested stop, while the entry
// keeps its state and its tools. The loss is not a requested stop, so
// craft's own stopped hook (token revocation) does not run for it.
func TestHostOnStoppedReportsTerminalProcessLoss(t *testing.T) {
	plugin := startStopPluginHost(t, 0)
	events := &stopEvents{}
	plugin.host.OnStopped(events.record)

	var mu sync.Mutex
	var internal []string
	plugin.host.SetPluginHooks(nil, func(id string) {
		mu.Lock()
		internal = append(internal, id)
		mu.Unlock()
	})

	pids := plugin.waitForPIDs(t, 1, 15*time.Second)
	pid, err := strconv.Atoi(pids[0])
	if err != nil {
		t.Fatalf("helper pid %q: %v", pids[0], err)
	}
	// The command disappears while the process runs: the reconnect
	// after the kill cannot spawn it again.
	if err := os.Remove(filepath.Join(plugin.dir, "server")); err != nil {
		t.Fatalf("remove server: %v", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal helper: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		got := events.snapshot()
		if len(got) > 0 {
			if !reflect.DeepEqual(got, []string{"hello:" + string(StopFailed)}) {
				t.Fatalf("stop events = %v, want [hello:%s]", got, StopFailed)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no stop report after the plugin process died (pids %v)",
				plugin.helperPIDs(t))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The host leaves the plugin alone: no stop was performed, so the
	// entry is still running and its tools are still projected.
	plugin.host.mu.Lock()
	_, running := plugin.host.live["hello"]
	plugin.host.mu.Unlock()
	if !running {
		t.Fatal("the loss dropped the host's record of the plugin")
	}
	if len(plugin.host.ToolSet().Tools()) == 0 {
		t.Fatal("the loss dropped the plugin's tools")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(internal) != 0 {
		t.Fatalf("the internal stopped hook ran for a process loss: %v", internal)
	}
}

// holdFirstStop subscribes a reporting hook that stops in its first
// delivery of reason until the returned release channel is closed, so a
// test can pin what the host does while one report is being delivered.
// Every delivery is recorded, the held one included.
func holdFirstStop(
	reason StopReason,
	events *stopEvents,
) (hook func(string, StopReason), entered <-chan struct{}, release chan struct{}) {
	enteredCh := make(chan struct{})
	releaseCh := make(chan struct{})
	var hold sync.Once
	hook = func(id string, got StopReason) {
		if got == reason {
			hold.Do(func() {
				close(enteredCh)
				<-releaseCh
			})
		}
		events.record(id, got)
	}
	return hook, enteredCh, releaseCh
}

// TestHostOnStoppedReportsOneStopPerProcess covers the seam between a
// process that gave up on its own and the host stopping that same
// process. They are one event — the process is gone either way — so the
// application hears it once: whichever side records it first narrates
// it, and the other stays silent about that stop. The stop's own half
// still runs: the plugin leaves the live set, the record goes disabled,
// and the token of the process being let go of is revoked.
func TestHostOnStoppedReportsOneStopPerProcess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", contributionManifest("hello", "tools.provide"))
	writeContributionTree(t, root, "hello")
	host := startHost(t, newStoreOver(t, root), newSourceFactory().new)
	ctx := context.Background()

	// The loss is held in the middle of its delivery while the disable
	// runs, which is the interleaving the two share.
	events := &stopEvents{}
	hook, entered, release := holdFirstStop(StopFailed, events)
	host.OnStopped(hook)
	var mu sync.Mutex
	var internal []string
	host.SetPluginHooks(nil, func(id string) {
		mu.Lock()
		internal = append(internal, id)
		mu.Unlock()
	})
	if err := host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("enable hello: %v", err)
	}

	go host.notifyLost("hello")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the loss never reached its delivery")
	}
	if err := host.SetEnabled(ctx, "hello", false); err != nil {
		t.Fatalf("disable hello: %v", err)
	}
	close(release)

	want := []string{"hello:" + string(StopFailed)}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := events.snapshot(); len(got) > 0 {
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stop events = %v, want %v", got, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no stop report for a plugin whose process gave up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	entry, ok := host.Store().Entry("hello")
	if !ok || entry.Enabled {
		t.Fatalf("entry after the stop = %+v, %v, want it recorded disabled",
			entry, ok)
	}
	if host.isLive("hello") {
		t.Fatal("the host still holds a process for a plugin it stopped")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(internal) != 1 || internal[0] != "hello" {
		t.Fatalf("internal stopped hook ran for %v, want one stop of hello", internal)
	}
}

// TestHostOnStoppedIgnoresALossDuringTheStopsDelivery covers the other
// order of the same seam: a loss that arrives while the host's own stop
// is being reported is not a second event. The stop already took the
// plugin out of the live set, so there is no process left for the loss
// to be about.
func TestHostOnStoppedIgnoresALossDuringTheStopsDelivery(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", contributionManifest("hello", "tools.provide"))
	writeContributionTree(t, root, "hello")
	host := startHost(t, newStoreOver(t, root), newSourceFactory().new)
	ctx := context.Background()

	events := &stopEvents{}
	hook, entered, release := holdFirstStop(StopRequested, events)
	host.OnStopped(hook)
	if err := host.SetEnabled(ctx, "hello", true); err != nil {
		t.Fatalf("enable hello: %v", err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- host.SetEnabled(ctx, "hello", false) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop never reached its delivery")
	}
	host.notifyLost("hello")
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("disable hello: %v", err)
	}

	want := []string{"hello:" + string(StopRequested)}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("stop events = %v, want %v", got, want)
	}
}
