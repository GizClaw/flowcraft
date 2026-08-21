package delegation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// SessionProvider decides a delegated subagent's session ContextID and
// whether that identity is stable and needs persistence. Implementations
// are injected by the application. A nil provider falls back to the
// service default: mint a fresh ContextID per delegation, but only when a
// session manager is bound (see LocalService.runAt).
type SessionProvider interface {
	// CreateContextID returns the subagent session's ContextID. AgentID is
	// filled by the service from the validated request target, so a
	// provider can never introduce an identity mismatch. Returning an
	// error fails the delegation; returning an empty ContextID is a
	// validation error.
	CreateContextID(ctx context.Context, req AsyncRequest) (string, error)

	// Persistent reports whether the provider's ContextID is stable and
	// should survive as a resumable session. Providers deriving stable
	// ContextIDs (e.g. by caller+target) return true; providers minting a
	// fresh ContextID per delegation return false.
	Persistent() bool
}

// RandomSessionProvider mints a unique ContextID for every delegation and
// never persists: each delegated run is an ephemeral session with no
// history or resume state.
type RandomSessionProvider struct{}

// CreateContextID implements SessionProvider.
func (RandomSessionProvider) CreateContextID(context.Context, AsyncRequest) (string, error) {
	return newContextID(), nil
}

// Persistent implements SessionProvider.
func (RandomSessionProvider) Persistent() bool { return false }

// newContextID mints a unique, non-empty session ContextID.
func newContextID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return "ctx-" + hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("ctx-%d", time.Now().UnixNano())
}
