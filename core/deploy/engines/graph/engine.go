// Package graph registers the "graph" engine with a deployment.
//
// It is the opt-in half of core/deploy's kind table, split out for the reason
// core/runtime/backends is: a host that never runs a graph agent should not link
// the graph runtime, and core/deploy itself imports no engine at all. Importing
// this package is the decision to run one.
//
// The registration is a binding, not an implementation: reading the document,
// compiling it and building the node registry all belong to
// core/agent-graph/nodes, so this package only maps the settings onto it —
// exactly as the a2a engine maps its peer onto core/agent-a2a.
package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	agentgraph "github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
	"github.com/goccy/go-json"
)

// Engine returns the "graph" engine kind: a document compiled once per
// deployment, one runner per task.
//
// Options extend the node registry the engine builds, which is how a host adds
// its own node kinds — a script node, a provider-specific model wrapper — beside
// the built-in model, tool and condition.
func Engine(options ...nodes.Option) deploy.EngineKind {
	return deploy.EngineKind{
		Name:  "graph",
		Local: true,
		Decode: func(settings json.RawMessage) (any, error) {
			return decodeSettings(settings)
		},
		Fingerprint: fingerprint,
		Build: func(ctx context.Context, deps deploy.EngineDeps) (agent.RunnerFactory, error) {
			settings, err := decodeSettings(deps.Spec.Settings)
			if err != nil {
				return nil, err
			}
			open := append([]nodes.Option{}, options...)
			open = append(open, nodes.WithBlobStore(deps.Store))
			built, err := nodes.Open(ctx, deps.Runtime, settings.Path, open...)
			if err != nil {
				return nil, err
			}
			return func(_ context.Context, sessionID, taskID string) (agent.Runner, error) {
				return agentgraph.NewRunner(built, agentgraph.RunnerConfig{
					Store:     deps.Store,
					Identity:  deps.Fingerprint,
					SessionID: sessionID,
					TaskID:    taskID,
					Manifest:  deps.Manifest,
					Trace:     deps.Trace,
					Bodies:    deps.Bodies,
				}), nil
			}, nil
		},
	}
}

// graphSettings is the engine's written configuration.
type graphSettings struct {
	// Path is the graph document. It is read at load time and again at build
	// time; a relative path is relative to the process's working directory.
	Path string `json:"path"`
}

func decodeSettings(settings json.RawMessage) (graphSettings, error) {
	if len(settings) == 0 || string(settings) == "null" {
		return graphSettings{}, errdefs.Validationf("graph: settings.path is required")
	}
	decoded, err := decode.Decode[graphSettings](settings)
	if err != nil {
		return graphSettings{}, errdefs.Validationf("graph: settings: %v", err)
	}
	if strings.TrimSpace(decoded.Path) == "" {
		return graphSettings{}, errdefs.Validationf("graph: settings.path is required")
	}
	// Validate the document now so a wiring mistake fails at load, not when the
	// first task starts. Node kinds are checked at build, when the registry is
	// known.
	if err := nodes.Validate(context.Background(), decoded.Path); err != nil {
		return graphSettings{}, err
	}
	return decoded, nil
}

// fingerprint is the document's identity: its path and its bytes. Editing the
// file therefore invalidates the checkpoints taken under the old graph, which is
// the same rule a changed runtime or engine setting follows.
func fingerprint(settings json.RawMessage) string {
	var decoded graphSettings
	if err := json.Unmarshal(settings, &decoded); err != nil {
		// Decode refuses a settings blob that does not parse, so this is
		// unreachable through the loader. If it is reached anyway, the identity
		// must differ from every real document's rather than silently match the
		// hash of the empty path.
		return "invalid"
	}
	sum := sha256.Sum256([]byte(decoded.Path))
	if body, err := os.ReadFile(decoded.Path); err == nil {
		sum = sha256.Sum256(append([]byte(decoded.Path+"\x00"), body...))
	}
	return "sha256:" + hex.EncodeToString(sum[:])
}
