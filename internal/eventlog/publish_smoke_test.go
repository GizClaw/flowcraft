package eventlog

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPublishTaskSubmittedSmoke(t *testing.T) {
	var m MemoryAppender
	ctx := context.Background()
	_, err := PublishTaskSubmitted(ctx, &m, "rt-1", TaskSubmittedPayload{
		CardID:        "c1",
		TargetAgentID: "a1",
		Query:         "q",
		RuntimeID:     "rt-1",
	}, WithTraceIDs("tr", "sp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Last) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(m.Last))
	}
	env := m.Last[0]
	if env.Type != EventTypeTaskSubmitted {
		t.Fatalf("type %s", env.Type)
	}
	if env.Partition != PartitionRuntime("rt-1") {
		t.Fatalf("partition %s", env.Partition)
	}
	var p TaskSubmittedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.CardID != "c1" {
		t.Fatalf("payload %+v", p)
	}
}
