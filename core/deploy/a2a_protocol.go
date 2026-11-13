package deploy

import (
	"context"

	"github.com/GizClaw/flowcraft/core/deploy/a2aprotocol"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/goccy/go-json"
)

// A2AProtocol registers the "a2a" exposure: this agent reachable over A2A.
//
// The server, the executor and the task store live in deploy/a2aprotocol; this is
// only the registration that mounts them, kept here for one structural reason:
// that package needs [ProtocolDeps] and [ProtocolKind], so it imports this one, and
// a registration living inside it could never be offered by [BuiltinKinds] without
// a cycle. The dependency points one way — deploy knows the subpackage, never the
// reverse — which is the same shape A2AEngine has to agent-a2a.
//
// Handler options are host wiring (transport, interceptors), so they are captured
// here rather than living on the document.
func A2AProtocol(options ...a2asrv.RequestHandlerOption) ProtocolKind {
	return ProtocolKind{
		Name: "a2a",
		Decode: func(settings json.RawMessage) (any, error) {
			return a2aprotocol.DecodeSettings(settings)
		},
		Build: func(_ context.Context, deps ProtocolDeps) (Protocol, error) {
			return a2aprotocol.New(a2aprotocol.Config{
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
