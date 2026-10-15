package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// hangingTool is one plugin tool whose execution the test holds open, so
// a drain has something it can only wait for.
type hangingTool struct {
	name    string
	started chan struct{}
	release chan struct{}
}

func (t hangingTool) Definition() message.ToolDefinition {
	return message.ToolDefinition{Name: t.name}
}

func (t hangingTool) Execute(context.Context, string) (message.Content, error) {
	close(t.started)
	<-t.release
	return message.Content{}, nil
}

// TestToolSetCloseBoundsEachDrain pins the aggregator's own close path:
// each plugin's in-flight calls are waited for within the host's drain
// bound, so one call that never finishes cannot keep the aggregator — or
// the shutdown behind it — from finishing. The source is closed anyway,
// and what it was waiting for is reported.
func TestToolSetCloseBoundsEachDrain(t *testing.T) {
	t.Parallel()
	set := newToolSet(20 * time.Millisecond)
	held := hangingTool{
		name:    "hello__echo",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	if err := set.AddPlugin("hello", &fakeSource{tools: []tool.Tool{held}}); err != nil {
		t.Fatalf("AddPlugin: %v", err)
	}
	published := publishedTool(t, &Host{tools: set}, "hello__echo")
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = published.Execute(context.Background(), "{}")
	}()
	select {
	case <-held.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never started")
	}

	closed := make(chan error, 1)
	go func() { closed <- set.Close() }()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("Close reported nothing about the call it stopped waiting for")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for a call that never finishes")
	}
	close(held.release)
	<-callDone
}

// removalRecorder is a downstream registry that reports the moment the
// aggregator unpublishes a tool. The unpublishing happens after the
// drain's cutoff, so a test that waits for it knows the plugin refuses
// new calls — the one moment a drain cannot be observed through the
// aggregator itself.
type removalRecorder struct {
	removed chan string
}

func newRemovalRecorder() *removalRecorder {
	return &removalRecorder{removed: make(chan string, 16)}
}

func (r *removalRecorder) Add(tool.Tool) error { return nil }

func (r *removalRecorder) Remove(name string) {
	select {
	case r.removed <- name:
	default:
	}
}

// awaitRemoval waits until the recorder saw the named tool unpublished.
func (r *removalRecorder) awaitRemoval(t *testing.T, name string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-r.removed:
			if got == name {
				return
			}
		case <-deadline:
			t.Fatalf("tool %q was never unpublished", name)
		}
	}
}

// publishedTool returns one plugin tool the way a caller holds it: a
// value resolved before the stop, which is exactly the shape that makes
// a drain racy — a caller can call a tool the registries no longer
// publish.
func publishedTool(t *testing.T, host *Host, name string) tool.Tool {
	t.Helper()
	for _, candidate := range host.ToolSet().Tools() {
		if candidate.Definition().Name == name {
			return candidate
		}
	}
	t.Fatalf("tool %q is not published", name)
	return nil
}

// TestDrainRefusesCallsItDidNotCount pins what a stop does with a call
// that arrives after the drain began. The call is refused — the plugin
// is stopping — instead of joining a set the drain already stopped
// accounting for: a call admitted then would have the process closed
// under it, which is the one thing waiting for the calls in flight is
// supposed to prevent.
func TestDrainRefusesCallsItDidNotCount(t *testing.T) {
	t.Parallel()
	factory := newVersionFactory()
	factory.block = make(chan struct{})
	started := make(chan struct{})
	factory.startedCall = started
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	store := newStoreOver(t, root)
	host := startHost(t, store, factory.new)

	held := publishedTool(t, host, "hello__echo")
	recorder := newRemovalRecorder()
	host.ToolSet().Attach(recorder)
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = held.Execute(context.Background(), "{}")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never started")
	}

	// The disable drains what is in flight, so it is still waiting here.
	disableDone := make(chan error, 1)
	go func() {
		disableDone <- host.SetEnabled(context.Background(), "hello", false)
	}()

	// The unpublishing is the drain's own cutoff: the second call below
	// arrives after it, so the plugin is refusing new work by then.
	recorder.awaitRemoval(t, "hello__echo")
	refused := make(chan error, 1)
	go func() {
		_, err := held.Execute(context.Background(), "{}")
		refused <- err
	}()
	select {
	case err := <-refused:
		if !errdefs.IsConflict(err) {
			t.Fatalf("a call during the drain = %v, want a conflict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call during the drain was admitted and never came back")
	}
	select {
	case err := <-disableDone:
		t.Fatalf("the disable returned %v while a call was still in flight", err)
	default:
	}

	close(factory.block)
	<-callDone
	select {
	case err := <-disableDone:
		if err != nil {
			t.Fatalf("disable: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the disable never returned after the call ended")
	}
	if entry, ok := store.Entry("hello"); !ok || entry.Enabled {
		t.Fatalf("entry after the disable = %+v, %v, want it disabled", entry, ok)
	}
}

// TestCloseBoundsTheDrainItWaitsFor pins that closing the host is
// bounded by the drain timeout. One call that never finishes is not
// allowed to keep the process — or the shutdown behind it — from
// finishing: the drain gives up, reports, and the source is closed
// anyway.
func TestCloseBoundsTheDrainItWaitsFor(t *testing.T) {
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

	held := publishedTool(t, host, "hello__echo")
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = held.Execute(context.Background(), "{}")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never started")
	}

	closed := make(chan error, 1)
	go func() { closed <- host.Close() }()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("Close reported nothing about the call it stopped waiting for")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for a call that never finishes")
	}
	if live := len(factory.closedVersions()); live != 1 {
		t.Fatalf("sources closed = %v, want the one the drain released", live)
	}

	close(factory.block)
	<-callDone
}
