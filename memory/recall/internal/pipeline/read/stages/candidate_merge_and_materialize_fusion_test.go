package stages

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

func TestFusionSourceFloorsGatesTimelineFloorToDirectDateIntent(t *testing.T) {
	direct := domain.QueryFeatures{Temporal: domain.QueryTemporalFeatures{
		HasIntent:  true,
		IntentKind: []domain.QueryTemporalIntentKind{domain.QueryTemporalIntentDate},
	}}
	directFloors := fusionSourceFloors(direct)
	if directFloors[planner.SourceRetrieval] != 5 || directFloors[planner.SourceTimeline] != 3 {
		t.Fatalf("direct date floors = %+v, want retrieval and timeline floors", directFloors)
	}

	rangeOnly := domain.QueryFeatures{Temporal: domain.QueryTemporalFeatures{
		HasIntent:  true,
		IntentKind: []domain.QueryTemporalIntentKind{domain.QueryTemporalIntentDate, domain.QueryTemporalIntentRange},
	}}
	rangeFloors := fusionSourceFloors(rangeOnly)
	if rangeFloors[planner.SourceRetrieval] != 5 {
		t.Fatalf("range floors should keep retrieval floor, got %+v", rangeFloors)
	}
	if _, ok := rangeFloors[planner.SourceTimeline]; ok {
		t.Fatalf("range floors should not reserve timeline slots without explicit bounds, got %+v", rangeFloors)
	}
}
