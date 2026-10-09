package plugin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/tool"
)

// TestDisableWaitsForTheStartItRaced covers the interleaving a user can
// create from a plugin list: a disable that arrives while the enable is
// still spawning the process. Start and stop of one plugin are
// serialized, so the disable stops the process the enable created
// instead of racing past it and leaving an untracked child behind.
func TestDisableWaitsForTheStartItRaced(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"], "mcp": {"command": "true"}
	}`)
	store := newStoreOver(t, root)

	opening := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	closed := 0
	factory := func(
		_ context.Context,
		_ Entry,
		_ map[string]string,
		_ string,
	) (Source, error) {
		once.Do(func() { close(opening) })
		<-release
		return &fakeSource{
			tools: []tool.Tool{fakeTool{name: "hello__echo"}},
			close: func() {
				mu.Lock()
				closed++
				mu.Unlock()
			},
		}, nil
	}
	host, err := NewHost(HostOptions{Store: store, NewSource: factory})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	ctx := context.Background()

	startDone := make(chan error, 1)
	go func() { startDone <- host.SetEnabled(ctx, "hello", true) }()
	<-opening

	stopDone := make(chan error, 1)
	go func() { stopDone <- host.SetEnabled(ctx, "hello", false) }()
	select {
	case err := <-stopDone:
		t.Fatalf("the disable overtook the start: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := <-startDone; err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("disable: %v", err)
	}
	mu.Lock()
	got := closed
	mu.Unlock()
	if got != 1 {
		t.Fatalf("the plugin process was closed %d times, want it stopped once",
			got)
	}
	entry, ok := store.Entry("hello")
	if !ok || entry.Enabled {
		t.Fatalf("entry after the disable = %+v, %v; want it disabled", entry, ok)
	}
	if tools := host.ToolSet().Tools(); len(tools) != 0 {
		t.Fatalf("tools still published after the disable: %v", tools)
	}
}
