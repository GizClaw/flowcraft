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
		Decode: func(ctx context.Context, settings json.RawMessage, sources deploy.SourceOpener) (any, error) {
			decoded, _, err := openSettings(ctx, settings, sources)
			if err != nil {
				return nil, err
			}
			return decoded, nil
		},
		Fingerprint: fingerprint,
		Build: func(ctx context.Context, deps deploy.EngineDeps) (agent.RunnerFactory, error) {
			_, document, err := openSettings(ctx, deps.Spec.Settings, deps.Sources)
			if err != nil {
				return nil, err
			}
			open := append([]nodes.Option{}, options...)
			open = append(open, nodes.WithBlobStore(deps.Store))
			built, err := nodes.Open(ctx, deps.Runtime, document.Name, document.Body, open...)
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
	// Source is the graph document: the kind its bytes come from, and that kind's
	// settings. It is read at load time for validation, again for the
	// fingerprint, and once more when the graph is compiled — the same three
	// reads a path used to cost.
	Source *deploy.LayerSourceDocument `json:"source"`
}

// openSettings decodes the settings and opens the document they name, validating
// it on the way: a wiring mistake fails at load, not when the first task starts.
// Node kinds are still checked at build, when the registry is known.
func openSettings(ctx context.Context, settings json.RawMessage, sources deploy.SourceOpener) (graphSettings, deploy.LayerSource, error) {
	const missing = "graph: settings.source is required"
	if len(settings) == 0 || string(settings) == "null" {
		return graphSettings{}, deploy.LayerSource{}, errdefs.Validationf(missing)
	}
	decoded, err := decode.Decode[graphSettings](settings)
	if err != nil {
		return graphSettings{}, deploy.LayerSource{}, errdefs.Validationf("graph: settings: %v", err)
	}
	if decoded.Source == nil {
		return graphSettings{}, deploy.LayerSource{}, errdefs.Validationf(missing)
	}
	if sources == nil {
		return graphSettings{}, deploy.LayerSource{}, errdefs.Validationf(
			"graph: settings.source: this host registered no source table")
	}
	document, err := sources(ctx, decoded.Source)
	if err != nil {
		return graphSettings{}, deploy.LayerSource{}, err
	}
	if err := nodes.Validate(ctx, document.Name, document.Body); err != nil {
		return graphSettings{}, deploy.LayerSource{}, err
	}
	return decoded, document, nil
}

// fingerprint is the document's identity: the source it names and its bytes.
// Editing the document therefore invalidates the checkpoints taken under the old
// graph, which is the same rule a changed runtime or engine setting follows.
func fingerprint(ctx context.Context, settings json.RawMessage, sources deploy.SourceOpener) string {
	var decoded graphSettings
	if err := json.Unmarshal(settings, &decoded); err != nil || decoded.Source == nil || sources == nil {
		// Decode refuses a settings blob that does not parse, so this is
		// unreachable through the loader. If it is reached anyway, the identity
		// must differ from every real document's rather than silently match the
		// hash of an empty one.
		return "invalid"
	}
	document, err := sources(ctx, decoded.Source)
	if err != nil {
		// Decode opened the same source successfully, so a failure here is a
		// source that disappeared between the two calls: the identity has to
		// differ from the old one, which is what an unreadable document means.
		return "invalid"
	}
	body, err := document.Read(ctx)
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(append([]byte(document.Name+"\x00"), body...))
	return "sha256:" + hex.EncodeToString(sum[:])
}
