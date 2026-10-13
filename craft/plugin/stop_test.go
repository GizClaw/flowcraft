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

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
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
		writePlugin(t, root, id, contributionManifest(id, "mcp:provide"))
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

// TestPluginStopHelper is the stdio server of the loss tests: it serves
// one tool, records its pid where the test can find it, and can exit on
// its own when the test asks for a plugin that crashes.
func TestPluginStopHelper(t *testing.T) {
	if os.Getenv("CRAFT_PLUGIN_STOP_HELPER") != "1" {
		t.Skip("plugin stop helper")
	}
	recordHelperPID(t)
	if raw := os.Getenv("CRAFT_PLUGIN_STOP_HELPER_EXIT_MS"); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms <= 0 {
			t.Fatalf("invalid CRAFT_PLUGIN_STOP_HELPER_EXIT_MS %q", raw)
		}
		time.AfterFunc(time.Duration(ms)*time.Millisecond, func() { os.Exit(0) })
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name: "stop-helper", Version: "0.1.0",
	}, nil)
	server.AddTool(&mcpsdk.Tool{
		Name:        "echo",
		InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}},
		}, nil
	})
	if err := server.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		t.Fatalf("helper server: %v", err)
	}
}

// recordHelperPID appends the helper's pid to the plugin data
// directory, so a test can kill the process the way a crash would and
// count how many children the host spawned.
func recordHelperPID(t *testing.T) {
	t.Helper()
	dir := os.Getenv("CRAFT_PLUGIN_DATA_DIR")
	if dir == "" {
		return
	}
	file, err := os.OpenFile(
		filepath.Join(dir, "pids"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open pid file: %v", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		t.Fatalf("record pid: %v", err)
	}
}

// stopTestPlugin is one running plugin whose stdio server is this test
// binary re-executed, plus the paths the loss tests need.
type stopTestPlugin struct {
	host     *Host
	dir      string
	dataRoot string
}

// startStopPluginHost scans one plugin, starts the host and returns it
// once the plugin's tools are published. A positive exitMS makes the
// helper exit on its own that many milliseconds after every start,
// which is what the transient-loss test needs.
func startStopPluginHost(t *testing.T, exitMS int) *stopTestPlugin {
	t.Helper()
	if os.Getenv("CRAFT_PLUGIN_STOP_HELPER") == "1" {
		t.Skip("running as helper")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "hello")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "server")); err != nil {
		t.Fatalf("symlink helper: %v", err)
	}
	env := `{"CRAFT_PLUGIN_STOP_HELPER": "1"}`
	if exitMS > 0 {
		env = `{"CRAFT_PLUGIN_STOP_HELPER": "1",` +
			`"CRAFT_PLUGIN_STOP_HELPER_EXIT_MS": "` + strconv.Itoa(exitMS) + `"}`
	}
	manifest := `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"],
		"mcp": {
			"command": "./server",
			"args": ["-test.run=TestPluginStopHelper"],
			"env": ` + env + `
		}
	}`
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	dataRoot := t.TempDir()
	store, err := NewStore(Options{
		Roots:       []Root{{Path: root}},
		StateDir:    t.TempDir(),
		DataDirRoot: dataRoot,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	host, err := NewHost(HostOptions{Store: store})
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
	_, running := plugin.host.sources["hello"]
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

// TestHostOnStoppedSilentForTransientProcessLoss pins the guard against
// flapping: a plugin whose process dies and is reconnected in place
// reports nothing, and its tools keep working.
func TestHostOnStoppedSilentForTransientProcessLoss(t *testing.T) {
	plugin := startStopPluginHost(t, 300)
	events := &stopEvents{}
	plugin.host.OnStopped(events.record)

	// The helper exits on its own; the reconnect spawns a fresh child,
	// which records a second pid.
	plugin.waitForPIDs(t, 2, 20*time.Second)

	echo := waitForTool(t, plugin.host, "hello__echo")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := echo.Execute(context.Background(), `{}`); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the plugin never served a call again after the reconnect")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("a transient process loss was reported as %v, want silence", got)
	}
}
