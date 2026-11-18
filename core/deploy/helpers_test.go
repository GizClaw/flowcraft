package deploy

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/goccy/go-json"
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

// builtinKinds is the kind table these tests load documents against.
//
// The real registrations are opt-in packages (deploy/engines/*,
// deploy/protocols/a2a, deploy/builtin) that import this one, so an in-package
// test cannot import them back. What the loader needs from a kind is its name, its
// local-or-remote answer and a Decode that validates the settings; the engines
// themselves are tested where they live, and a2aKind stands in for the protocol.
func builtinKinds() Kinds {
	return Kinds{}.
		WithEngines(
			EngineKind{Name: "a2a"},
			EngineKind{Name: "graph", Local: true},
		).
		WithProtocols(a2aKind()).
		WithSources(fileSource())
}

// fileSource is the file layer source as the document sees it: settings.path,
// named after the path it reads. The registration these tests exercise end to end
// is deploy/builtin's, and it is tested from the external test package.
func fileSource() SourceKind {
	return SourceKind{
		Name: "file",
		Open: func(_ context.Context, settings json.RawMessage) (Source, error) {
			// Strict, like core/deploy/sources.File: a hand copy that accepted
			// unknown keys would keep these tests green while the real
			// registration refused the same document.
			decoded, err := decode.Decode[struct {
				Path string `json:"path"`
			}](settings)
			if err != nil {
				return Source{}, err
			}
			if decoded.Path == "" {
				return Source{}, errors.New("file: settings.path is required")
			}
			return Source{Name: decoded.Path, Body: config.File(decoded.Path)}, nil
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

const runtimeLayerYAML = `
version: 1
sandbox: {backend: local, writes: ["/work"]}
files: {roots: ["/work"]}
inference:
  providers: [{name: p, driver: stub}]
  models: [{provider: p, model: m}]
`

// platformPaths rewrites the unix placeholders a fixture is written with, so the
// same document is absolute on both platforms: "/work" is not absolute on
// Windows, where a path needs a volume.
func platformPaths(text string) string {
	return strings.NewReplacer(
		"/work", pathtest.Abs("work"),
		"/tmp", pathtest.Abs("tmp"),
	).Replace(text)
}

func runtimeLayer() string { return platformPaths(runtimeLayerYAML) }

func writeLayer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(runtimeLayer()), 0o600); err != nil {
		t.Fatalf("write layer: %v", err)
	}
	return path
}
