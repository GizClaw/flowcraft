package a2a

import (
	"context"

	"github.com/GizClaw/flowcraft/core/deploy"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/goccy/go-json"
)

// Protocol returns the "a2a" protocol kind: this agent reachable over A2A.
//
// It is here rather than in core/deploy because a registration belongs with the
// implementation, the way an engine registration belongs with its engine. The
// difference is which way the import has to point:
//
//   - an engine's implementation is a lower layer (core/agent-graph,
//     core/agent-a2a) that must not import core/deploy, so its registration
//     lives above them, in deploy/engines/*;
//   - this server is assembled from values a deployment already has, so it can
//     import deploy and register itself, and nothing in core/deploy has to know
//     it exists.
//
// A host that wants one engine and this exposure takes exactly those two, plus a
// source: the deployment document itself is read through the source table.
//
//	kinds := deploy.Kinds{}.
//	    WithEngines(enginesgraph.Engine()).
//	    WithProtocols(protocolsa2a.Protocol()).
//	    WithSources(sources.File())
//
// Handler options are host wiring (transport, interceptors), so they are captured
// here rather than living on the document.
func Protocol(options ...a2asrv.RequestHandlerOption) deploy.ProtocolKind {
	return deploy.ProtocolKind{
		Name: "a2a",
		Decode: func(settings json.RawMessage) (any, error) {
			return DecodeSettings(settings)
		},
		Build: func(_ context.Context, deps deploy.ProtocolDeps) (deploy.Protocol, error) {
			return New(Config{
				Name:        deps.Name,
				Settings:    deps.Spec.Settings,
				Agent:       deps.Agent,
				Store:       deps.Store,
				Fingerprint: deps.Fingerprint,
				ArtifactURL: deps.ArtifactURL,
				Options:     options,
			})
		},
	}
}
