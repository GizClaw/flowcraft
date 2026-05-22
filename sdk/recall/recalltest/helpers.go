package recalltest

import (
	"strings"

	"github.com/GizClaw/flowcraft/sdk/recall"
)

func conformanceScope() recall.Scope {
	return recall.Scope{RuntimeID: "rt", UserID: "u1"}
}

func gotIDs(facts []recall.TemporalFact) string {
	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, f.ID)
	}
	return strings.Join(ids, ",")
}
