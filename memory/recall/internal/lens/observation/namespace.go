package observation

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	retrievalns "github.com/GizClaw/flowcraft/memory/retrieval/namespace"
)

var namespacePrefix = retrievalns.MustRegister("recallobs")

func NamespaceFor(s domain.Scope) string {
	if s.UserID != "" {
		return namespacePrefix.UserScope(s.RuntimeID, s.UserID)
	}
	return namespacePrefix.GlobalScope(s.RuntimeID)
}
