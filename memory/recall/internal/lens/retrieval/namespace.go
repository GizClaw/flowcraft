package retrieval

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	retrievalns "github.com/GizClaw/flowcraft/memory/retrieval/namespace"
)

// namespacePrefix owns the v2 retrieval namespace. It is distinct
// from the v1 "ltm" prefix so v1 and v2 indexes can coexist during
// the migration window without colliding on namespace strings.
var namespacePrefix = retrievalns.MustRegister("recall")

// NamespaceFor maps a scope to its v2 retrieval namespace. Mirrors
// the v1 scheme (per-user when UserID is set, runtime-global
// otherwise) so the namespace convention stays consistent across
// the SDK.
func NamespaceFor(s domain.Scope) string {
	if s.UserID != "" {
		return namespacePrefix.UserScope(s.RuntimeID, s.UserID)
	}
	return namespacePrefix.GlobalScope(s.RuntimeID)
}
