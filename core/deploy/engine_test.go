package deploy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/runtime"
)

type stubProtocol struct{ closed bool }

func (p *stubProtocol) Handler() http.Handler { return http.NotFoundHandler() }
func (p *stubProtocol) Close() error          { p.closed = true; return nil }

// The kind tables decide at Add time, so a hand-built Spec cannot slip past the
// document loader's checks.
func TestAddRejectsUnregisteredKinds(t *testing.T) {
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := manager.Add(context.Background(), Spec{
		Name: "a", Engine: EngineSpec{Kind: "nope"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(&scripted{})},
	}); err == nil || !strings.Contains(err.Error(), "react") {
		t.Fatalf("err = %v, want the registered engines listed", err)
	}

	if _, err := manager.Add(context.Background(), Spec{
		Name: "b", Engine: EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(&scripted{})},
		Protocol:       &ProtocolSpec{Kind: "nope"},
	}); err == nil {
		t.Fatal("an unregistered protocol was accepted")
	}
}

// A registered protocol is built at Add and surfaced on the agent; without one
// there is no handler, which is how "not exposed" stays visible.
func TestProtocolRegistrationBuildsTheSurface(t *testing.T) {
	surface := &stubProtocol{}
	manager := New(WithProtocols(Protocol{
		Name: "stub",
		Build: func(ProtocolSpec, *Agent) (protocol, error) {
			return surface, nil
		},
	}))
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "react"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(&scripted{})},
		Protocol:       &ProtocolSpec{Kind: "stub"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if deployed.Handler() == nil {
		t.Error("the protocol did not expose a handler")
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !surface.closed {
		t.Error("the protocol was not closed with the agent")
	}
}

func TestEngineNamesListsTheBuiltins(t *testing.T) {
	names := New().EngineNames()
	joined := strings.Join(names, ",")
	if joined != "a2a,react" {
		t.Errorf("engines = %q, want the built-ins", joined)
	}
}
