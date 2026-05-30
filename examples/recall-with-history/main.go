// Example: caller-side composition of history + recall + save.
//
// Recall does not hold a history.Store reference. The caller fetches
// recent messages and optional recall anchors, then passes them on
// SaveRequest.RecentMessages and SaveRequest.ExistingFactsAnchor.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/GizClaw/flowcraft/sdk/recall"
)

func main() {
	ctx := context.Background()
	mem, err := recall.New()
	if err != nil {
		log.Fatal(err)
	}
	defer mem.Close()

	scope := recall.Scope{RuntimeID: "demo-rt", UserID: "alice"}

	// Pretend these came from history.GetRecentMessages(scope, k).
	recent := []recall.Message{
		{Role: "user", Speaker: "alice", Text: "I moved to Paris last year."},
	}

	// Pretend these came from mem.Recall(scope, anchorQuery) for dedup.
	var anchors []recall.TemporalFact

	_, err = mem.Save(ctx, scope, recall.SaveRequest{
		Turns: []recall.TurnContext{{
			Role: "user", Speaker: "alice", Text: "I now live in Lyon.",
		}},
		RecentMessages:      recent,
		ExistingFactsAnchor: anchors,
	})
	if err != nil {
		log.Fatal(err)
	}

	hits, err := mem.Recall(ctx, scope, recall.Query{Text: "where does alice live", Limit: 5})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("recall returned %d hits\n", len(hits))
}
