package plugin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
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

// TestHostStartBehindACloseRevokesWhatItMinted covers the other race a
// user can create: a close that lands while the enable is still opening
// its process. The source is closed either way — the host may not leave
// a process behind a closed host — but closing a source is not what
// revokes the token minted for it: the stopped hook is. A start that
// ends without publishing is exactly the one whose token would otherwise
// sit in the host until the Craft it belongs to is gone.
func TestHostStartBehindACloseRevokesWhatItMinted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"], "mcp": {"command": "true"}
	}`)
	store := newStoreOver(t, root)
	if err := store.SetEnabled("hello", false); err != nil {
		t.Fatalf("disable before start: %v", err)
	}

	opening := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	factory := func(
		_ context.Context,
		_ Entry,
		_ map[string]string,
		_ string,
	) (Source, error) {
		once.Do(func() { close(opening) })
		<-release
		return &fakeSource{tools: []tool.Tool{fakeTool{name: "hello__echo"}}}, nil
	}
	host, err := NewHost(HostOptions{Store: store, NewSource: factory})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	hook := &hookRecorder{}
	minted := make(chan struct{}, 1)
	host.SetPluginHooks(func(Entry, uint64, map[string]string) error {
		minted <- struct{}{}
		return nil
	}, hook.record)
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx := context.Background()
	startDone := make(chan error, 1)
	go func() { startDone <- host.SetEnabled(ctx, "hello", true) }()
	select {
	case <-opening:
	case <-time.After(5 * time.Second):
		t.Fatal("the enable never reached its source")
	}
	select {
	case <-minted:
	case <-time.After(5 * time.Second):
		t.Fatal("the environment hook never ran for the process being opened")
	}
	if err := host.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(release)
	if err := <-startDone; !errdefs.IsNotAvailable(err) {
		t.Fatalf("enable behind a close = %v, want NotAvailable", err)
	}
	if got := hook.count(); got != 1 {
		t.Fatalf("stopped hook calls = %d, want 1: the token minted for the "+
			"process this start opened has to be revoked", got)
	}
	if running(host, "hello") {
		t.Fatal("the closed host records a process")
	}
}

// TestHostFailedOpenRevokesWhatItMinted covers the mint an open cannot
// take back by itself: the environment hook runs before the source is
// opened — it is where the process is prepared — so a factory that fails
// leaves a token minted for a process that was never there. The enable
// reports the factory's error, and the token goes with the start that
// never happened.
func TestHostFailedOpenRevokesWhatItMinted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", `{
		"id": "hello", "version": "0.1.0",
		"permissions": ["mcp:provide"], "mcp": {"command": "true"}
	}`)
	store := newStoreOver(t, root)
	if err := store.SetEnabled("hello", false); err != nil {
		t.Fatalf("disable before start: %v", err)
	}
	host, err := NewHost(HostOptions{
		Store: store,
		NewSource: func(
			context.Context, Entry, map[string]string, string,
		) (Source, error) {
			return nil, errors.New("plugin hello: spawn: no such command")
		},
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	hook := &hookRecorder{}
	minted := false
	host.SetPluginHooks(func(Entry, uint64, map[string]string) error {
		minted = true
		return nil
	}, hook.record)
	if err := host.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = host.SetEnabled(context.Background(), "hello", true)
	if err == nil || !strings.Contains(err.Error(), "no such command") {
		t.Fatalf("enable of a plugin whose open fails = %v, want the factory's error", err)
	}
	if !minted {
		t.Fatal("the environment hook never ran before the open")
	}
	if got := hook.count(); got != 1 {
		t.Fatalf("stopped hook calls = %d, want 1: the token minted for an "+
			"open that failed has to be revoked", got)
	}
	if running(host, "hello") {
		t.Fatal("the failed open left a process recorded")
	}
}
