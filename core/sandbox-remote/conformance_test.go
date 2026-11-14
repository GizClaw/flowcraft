package remote_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox-remote"
	"github.com/GizClaw/flowcraft/core/sandbox/sandboxtest"
)

// TestConformance is the acceptance test for the whole remote stack: a real
// runner behind a real HTTP listener, driven through the client by the same
// suite every backend runs against itself.
//
// It is the point of the suite: if the wire keeps the contract, remote(local)
// passes the same assertions local does, and nothing here has to restate what a
// session is supposed to do.
func TestConformance(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T, root string) sandboxtest.Harness {
		t.Helper()
		return sandboxtest.Harness{
			Runner: newRemoteRunner(t, root),
			Root:   root,
		}
	})
}

// newRemoteRunner wires local -> wire -> client and returns the client side.
func newRemoteRunner(t *testing.T, root string) *remote.Runner {
	t.Helper()

	inner := newInner(t, root)
	t.Cleanup(func() { _ = inner.Close() })

	server, err := remote.NewServer(inner, remote.WithServerToken(testToken))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)

	runner, err := remote.New(context.Background(), listener.URL, remote.WithToken(testToken))
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	return runner
}

const testToken = "test-token"
