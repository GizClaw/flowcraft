package a2aprotocol

import (
	"context"

	"github.com/GizClaw/flowcraft/core/deploy"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/goccy/go-json"
)

// Kind is the A2A protocol registration: an agent reachable over A2A.
//
// It is a registration, not a default: a manager only exposes what a
// deployment's document names. Handler options are host wiring, so they are
// captured here instead of living on the spec.
//
// The registration lives with the implementation rather than in deploy, so
// deploy stays protocol-agnostic: it knows the ProtocolKind shape and nothing
// about the A2A SDK. A host composes the two explicitly:
//
//	deploy.New(deploy.BuiltinKinds().WithProtocols(a2aprotocol.Kind()), host)
func Kind(options ...a2asrv.RequestHandlerOption) deploy.ProtocolKind {
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
