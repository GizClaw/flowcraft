package verify

import (
	"context"
	"strings"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func projectionEvidence(name string) ProjectionEvidence {
	return ProjectionEvidence{
		Name: name, StoredSourceDigest: "source", ComputedSourceDigest: "source",
		StoredBuildDigest: "build", ComputedBuildDigest: "build",
		StoredProducer: "okapi-bm25-v2+k1=1.2,b=0.75", ExpectedProducer: "okapi-bm25-v2+k1=1.2,b=0.75",
	}
}

// A lane whose state an earlier producer wrote is a rebuild, not a digest
// problem: the digests are recomputed from the stored state and match whatever
// wrote it, so the identity is the only evidence of staleness -- and an
// unstamped state is the same condition, not a healthy one.
func TestInspectReportsAStaleProducerAsARebuild(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}

	plan, err := InspectContext(ctx, scope, Input{Projections: []ProjectionEvidence{projectionEvidence("bm25")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("a lane the running producer wrote planned %+v", plan.Actions)
	}

	unstamped := projectionEvidence("bm25")
	unstamped.StoredProducer = ""
	plan, err = InspectContext(ctx, scope, Input{Projections: []ProjectionEvidence{unstamped}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionRebuild || plan.Actions[0].Target != "projection:bm25" {
		t.Fatalf("unstamped projection planned %+v", plan.Actions)
	}
	if !strings.Contains(plan.Actions[0].Evidence, "stored unstamped") {
		t.Fatalf("unstamped evidence = %q", plan.Actions[0].Evidence)
	}

	older := projectionEvidence("bm25")
	older.StoredProducer = "okapi-bm25-v1"
	plan, err = InspectContext(ctx, scope, Input{Projections: []ProjectionEvidence{older}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionRebuild || plan.Actions[0].Target != "projection:bm25" {
		t.Fatalf("stale projection planned %+v", plan.Actions)
	}
	if !strings.Contains(plan.Actions[0].Evidence, "producer version mismatch: stored okapi-bm25-v1, expected okapi-bm25-v2+k1=1.2,b=0.75") {
		t.Fatalf("stale evidence = %q", plan.Actions[0].Evidence)
	}
}
