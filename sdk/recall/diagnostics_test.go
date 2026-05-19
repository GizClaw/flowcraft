package recall

import (
	"context"
	"testing"
)

func TestAttributeRecallTrace_PublicWrapper(t *testing.T) {
	attrs := AttributeRecallTrace(RecallTrace{
		Drops: []CandidateDrop{{
			Reason: DropStaleFact,
			FactID: "f1",
			Source: "retrieval",
		}},
	})
	if len(attrs) != 1 || attrs[0].Stage != FailureProjection {
		t.Fatalf("attrs = %+v", attrs)
	}
}

func TestRepairPlanFromTrace(t *testing.T) {
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	plan := RepairPlanFromTrace(scope, RecallTrace{
		Drops: []CandidateDrop{{Reason: DropStaleFact, FactID: "stale"}},
	})
	if len(plan.FactIDs) != 1 || plan.FactIDs[0] != "stale" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestWithGovernance_RejectsOnSave(t *testing.T) {
	g := DefaultGovernance()
	g.Write = rejectAllWritePolicy{}
	mem, err := New(WithGovernance(g))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()

	res, err := mem.Save(context.Background(), Scope{RuntimeID: "rt", UserID: "u1"}, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.FactIDs) != 0 {
		t.Fatalf("governance reject should yield empty save, got %+v", res)
	}
}

type rejectAllWritePolicy struct{}

func (rejectAllWritePolicy) Apply(TemporalFact) (TemporalFact, bool) {
	return TemporalFact{}, false
}
