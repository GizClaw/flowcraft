package a2a

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/goccy/go-json"

	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// Config is one remote agent as a deployment's program.
//
// It holds only what the deployment decides. The session's task, store and fingerprint arrive with
// each turn, so a program is shared by every session that selects this peer.
type Config struct {
	// Peer is the remote agent's endpoint.
	Peer string
	// Auth is the credential for reaching it, as written (JSON).
	Auth json.RawMessage
}

// Program is a remote A2A agent driven as an engine.
//
// It is the deployment-scoped half: the client is one connection policy for every session, and a
// session gets a [driver] that remembers the peer's task and context. That split is what makes it
// interchangeable with a local engine — a deployment selects either by name and the host opens
// sessions the same way.
type Program struct {
	client *a2aclient.Client
}

var _ agent.Program = (*Program)(nil)

// New builds the client for the peer.
//
// The client is the program's, not the session's: one connection policy serves every conversation
// the deployment has with this peer. A malformed config fails here, at install time; reachability
// cannot, because the transport resolves the card on the first call — which the host reports as the
// first turn's failure, where a caller can act on it.
func New(ctx context.Context, cfg Config) (*Program, error) {
	client, err := newClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Program{client: client}, nil
}

// Open binds the program to one session.
//
// It cannot fail and it dials nothing: the peer connection is already there, and the session's own
// identity — the peer's task id, the context id it assigned — is read from the store on the first
// turn, where the store is known.
func (p *Program) Open(info agent.SessionInfo) (agent.Driver, error) {
	if strings.TrimSpace(info.SessionID) == "" {
		return nil, errdefs.Validationf("a2a: a session id is required")
	}
	return &driver{client: p.client, session: info.SessionID}, nil
}

// Close releases the program. The JSON-RPC client holds no pool of its own, so there is nothing to
// drain; a kept-alive connection is the transport's to retire.
func (p *Program) Close() error { return nil }
