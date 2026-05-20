package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

type captureEvolution struct {
	saves   int
	recalls int
	trace   RecallTrace
}

func (c *captureEvolution) AfterSave(context.Context, domain.Scope, []string) error {
	c.saves++
	return nil
}

func (c *captureEvolution) AfterRecall(_ context.Context, _ domain.Scope, trace domain.RecallTrace) error {
	c.recalls++
	c.trace = trace
	return nil
}

func TestWithEvolution_HooksSaveAndRecall(t *testing.T) {
	ev := &captureEvolution{}
	mem, err := New(WithEvolution(ev))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "hello", Entities: []string{"hello"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Recall(context.Background(), scope, Query{Entities: []string{"hello"}, Limit: 3}); err != nil {
		t.Fatal(err)
	}
	if ev.saves != 1 || ev.recalls != 1 {
		t.Fatalf("evolution hooks = saves %d recalls %d", ev.saves, ev.recalls)
	}
	if ev.trace.Materialized == 0 || ev.trace.FusedCandidates == 0 || len(ev.trace.Sources) == 0 {
		t.Fatalf("evolution recall trace was not populated: %+v", ev.trace)
	}
}
