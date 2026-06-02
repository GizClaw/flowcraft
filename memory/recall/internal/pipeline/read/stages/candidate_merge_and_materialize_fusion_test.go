package stages

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

func TestFusionSourceFloorsFollowRecallStrategy(t *testing.T) {
	temporalFloors := fusionSourceFloors(domain.IntentRoute{Strategy: domain.RecallStrategyTemporal})
	if temporalFloors[planner.SourceRetrieval] != 5 || temporalFloors[planner.SourceTimeline] != 3 {
		t.Fatalf("temporal floors = %+v, want retrieval and timeline floors", temporalFloors)
	}

	setFloors := fusionSourceFloors(domain.IntentRoute{Strategy: domain.RecallStrategySet})
	if setFloors[planner.SourceRetrieval] != 5 {
		t.Fatalf("set floors should keep retrieval floor, got %+v", setFloors)
	}
	if _, ok := setFloors[planner.SourceTimeline]; ok {
		t.Fatalf("set floors should not reserve timeline slots, got %+v", setFloors)
	}
}

func TestMergeSourceFloorsPreservesNilDefaultWhenEmpty(t *testing.T) {
	if got := mergeSourceFloors(nil, nil); got != nil {
		t.Fatalf("empty source floors should remain nil so fuser applies defaults, got %+v", got)
	}
}
