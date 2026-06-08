package recall

import (
	"strconv"
	"strings"

	retrievalns "github.com/GizClaw/flowcraft/memory/retrieval/namespace"
)

// NamespaceFor returns the v2 retrieval namespace for a memory scope.
func NamespaceFor(s Scope) string {
	rt := retrievalns.Sanitize(s.RuntimeID)
	if s.UserID == "" {
		return "recall_" + rt + "__global"
	}
	user := retrievalns.Sanitize(s.UserID)
	var b strings.Builder
	b.Grow(len("recall_") + len(rt) + len("__u") + 5 + 1 + len(user))
	b.WriteString("recall_")
	b.WriteString(rt)
	b.WriteString("__u")
	b.WriteString(strconv.Itoa(len(user)))
	b.WriteByte('_')
	b.WriteString(user)
	return b.String()
}
