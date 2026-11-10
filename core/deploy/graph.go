package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"
)

// GraphEngine registers the "graph" engine: a document compiled once per
// deployment, one runner per task.
//
// It is a binding, not an implementation: reading the document, compiling it and
// building the node registry all belong to core/agent-graph/nodes, so this
// package only maps the settings onto it — exactly as the a2a engine maps its
// peer onto core/agent-a2a.
func GraphEngine(options ...nodes.Option) EngineKind {
	return EngineKind{
		Name:  "graph",
		Local: true,
		Decode: func(settings json.RawMessage) (any, error) {
			return decodeGraphSettings(settings)
		},
		Fingerprint: graphFingerprint,
		Build: func(ctx context.Context, deps EngineDeps) (agent.RunnerFactory, error) {
			settings, err := decodeGraphSettings(deps.Spec.Settings)
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
				return graph.NewRunner(built, graph.RunnerConfig{
					Store:     deps.Store,
					Identity:  deps.Fingerprint,
					SessionID: sessionID,
					TaskID:    taskID,
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

func decodeGraphSettings(settings json.RawMessage) (graphSettings, error) {
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

// graphFingerprint is the document's identity: its path and its bytes. Editing the
// file therefore invalidates the checkpoints taken under the old graph, which is
// the same rule a changed runtime or engine setting follows.
func graphFingerprint(settings json.RawMessage) string {
	var decoded graphSettings
	_ = json.Unmarshal(settings, &decoded)
	sum := sha256.Sum256([]byte(decoded.Path))
	if body, err := os.ReadFile(decoded.Path); err == nil {
		sum = sha256.Sum256(append([]byte(decoded.Path+"\x00"), body...))
	}
	return "sha256:" + hex.EncodeToString(sum[:])
}
