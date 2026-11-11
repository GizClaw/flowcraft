package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

// a2aKind stands in for the A2A registration: these tests exercise the
// document loader, not the protocol's translation.
func a2aKind() ProtocolKind {
	return ProtocolKind{
		Name: "a2a",
		Decode: func(json.RawMessage) (any, error) {
			return nil, nil
		},
		Build: func(context.Context, ProtocolDeps) (Protocol, error) {
			return stubProtocol{}, nil
		},
	}
}

type stubProtocol struct{}

func (stubProtocol) Handler() http.Handler { return http.NewServeMux() }
func (stubProtocol) Close() error          { return nil }

// engineFor registers one local engine kind that hands out the given runner.
func engineFor(name string, runner func() agent.Runner) EngineKind {
	return EngineKind{
		Name:  name,
		Local: true,
		Build: func(context.Context, EngineDeps) (agent.RunnerFactory, error) {
			return func(context.Context, string, string) (agent.Runner, error) {
				return runner(), nil
			}, nil
		},
	}
}

const runtimeLayer = `
version: 1
sandbox: {backend: local, writes: ["/work"]}
files: {roots: ["/work"]}
inference:
  providers: [{name: p, driver: stub}]
  models: [{provider: p, model: m}]
`

func writeLayer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(runtimeLayer), 0o600); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	return path
}

// rawJSON turns a Go literal into the JSON a settings block now is.
func rawJSON(value any) json.RawMessage {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}

// rawSettings renders an engine settings block the way the document would.
func rawSettings(value any) json.RawMessage {
	return rawJSON(value)
}
