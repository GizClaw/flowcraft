package recall

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

// TestRecallExplainer asserts that any Memory built by recall.New also
// satisfies RecallExplainer, and that RecallExplain threads
// SearchRequest.Debug into the underlying pipeline so callers get a
// non-nil SearchExecution back.
func TestRecallExplainer(t *testing.T) {
	ctx := context.Background()
	m, err := New(memidx.New(), WithRequireUserID())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := Scope{RuntimeID: "r1", UserID: "u1"}
	now := time.Now()
	for _, e := range []Entry{
		{ID: "go", Category: CategoryProfile, Content: "User is a Go developer", Keywords: []string{"go"}, UpdatedAt: now},
		{ID: "react", Category: CategoryProfile, Content: "User knows React", Keywords: []string{"react"}, UpdatedAt: now},
	} {
		if _, err := m.Add(ctx, scope, e); err != nil {
			t.Fatalf("add %s: %v", e.ID, err)
		}
	}

	rx, ok := m.(RecallExplainer)
	if !ok {
		t.Fatal("Memory built by New must implement RecallExplainer")
	}

	t.Run("debug zero leaves Execution nil", func(t *testing.T) {
		hits, exec, err := rx.RecallExplain(ctx, scope, Request{Query: "Go", TopK: 5})
		if err != nil {
			t.Fatal(err)
		}
		if exec != nil {
			t.Fatalf("expected nil Execution when Debug is zero, got %+v", exec)
		}
		if len(hits) == 0 {
			t.Fatal("expected at least one hit")
		}
	})

	t.Run("include lanes populates Execution.Lanes", func(t *testing.T) {
		hits, exec, err := rx.RecallExplain(ctx, scope, Request{
			Query: "Go",
			TopK:  5,
			Debug: retrieval.SearchDebug{IncludeLanes: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if exec == nil {
			t.Fatal("expected non-nil Execution when IncludeLanes=true")
		}
		if len(exec.Lanes) == 0 {
			t.Fatal("expected at least one lane in Execution")
		}
		if len(exec.Stages) != 0 {
			t.Fatalf("expected no stages when IncludeStages=false, got %+v", exec.Stages)
		}
		if len(hits) == 0 {
			t.Fatal("expected hits alongside Execution")
		}
	})

	t.Run("include stages records pipeline trace", func(t *testing.T) {
		_, exec, err := rx.RecallExplain(ctx, scope, Request{
			Query: "Go",
			TopK:  5,
			Debug: retrieval.SearchDebug{IncludeStages: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if exec == nil || len(exec.Stages) == 0 {
			t.Fatalf("expected stage trace, got %+v", exec)
		}
		for _, st := range exec.Stages {
			if st.Name == "" {
				t.Fatalf("stage missing name: %+v", st)
			}
		}
	})
}

// TestRecallExplainerLanesUseLaneKeyConstants asserts that the default
// pipeline lane labels surface as the canonical retrieval.LaneKey
// constants — guards against typo drift between
// pipeline.RetrieveMode/MultiRetrieve keys and retrieval.Lane*.
func TestRecallExplainerLanesUseLaneKeyConstants(t *testing.T) {
	ctx := context.Background()
	m, err := New(memidx.New(), WithRequireUserID())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	scope := Scope{RuntimeID: "r1", UserID: "u1"}
	if _, err := m.Add(ctx, scope, Entry{
		ID: "go", Category: CategoryProfile, Content: "User is a Go developer", Keywords: []string{"go"}, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	rx := m.(RecallExplainer)
	_, exec, err := rx.RecallExplain(ctx, scope, Request{
		Query: "Go", TopK: 5,
		Debug: retrieval.SearchDebug{IncludeLanes: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if exec == nil || len(exec.Lanes) == 0 {
		t.Fatalf("expected at least one lane, got %+v", exec)
	}
	// Default LTM(nil) recipe drops to BM25-only; lane key MUST be the
	// canonical retrieval.LaneBM25 constant.
	if exec.Lanes[0].Key != retrieval.LaneBM25 {
		t.Fatalf("expected lane key %q, got %q", retrieval.LaneBM25, exec.Lanes[0].Key)
	}
}

// TestRecallExplainerLanesCarryTook asserts the LaneResult.Took field
// is populated from the underlying Retrieve / MultiRetrieve stage so
// the recall_lane_duration histogram has non-zero samples to record.
func TestRecallExplainerLanesCarryTook(t *testing.T) {
	ctx := context.Background()
	m, err := New(memidx.New(), WithRequireUserID())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := Scope{RuntimeID: "r1", UserID: "u1"}
	if _, err := m.Add(ctx, scope, Entry{
		ID: "go", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	rx := m.(RecallExplainer)
	_, exec, err := rx.RecallExplain(ctx, scope, Request{
		Query: "Go", TopK: 5,
		Debug: retrieval.SearchDebug{IncludeLanes: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if exec == nil || len(exec.Lanes) == 0 {
		t.Fatalf("expected non-empty lanes, got %+v", exec)
	}
	for _, lane := range exec.Lanes {
		if lane.Took <= 0 {
			t.Fatalf("lane %q Took=%s, want > 0", lane.Key, lane.Took)
		}
	}
}

// TestRecallExplainerStageTraceCarriesDuration sandwiches a sleep stage
// inside the configured pipeline and asserts RecallExplain surfaces the
// stage's measured duration. This is the same trace
// recordStageDurations consumes for the recall_stage_duration histogram,
// so a non-zero Took here is sufficient evidence that metric path runs.
func TestRecallExplainerStageTraceCarriesDuration(t *testing.T) {
	ctx := context.Background()
	pipe := pipeline.New(
		pipeline.Retrieve{Lane: string(retrieval.LaneBM25), Spec: pipeline.RetrieveSpec{Mode: pipeline.ModeBM25, TopK: 5}},
		sleepyStage{name: "Sleepy", dur: 10 * time.Millisecond},
		pipeline.Limit{TopK: 5},
	)
	m, err := New(memidx.New(), WithRequireUserID(), WithPipeline(pipe))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := Scope{RuntimeID: "r1", UserID: "u1"}
	if _, err := m.Add(ctx, scope, Entry{
		ID: "go", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	rx := m.(RecallExplainer)
	_, exec, err := rx.RecallExplain(ctx, scope, Request{
		Query: "Go", TopK: 5,
		Debug: retrieval.SearchDebug{IncludeStages: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if exec == nil {
		t.Fatal("expected non-nil Execution")
	}
	var found bool
	for _, st := range exec.Stages {
		if st.Name != "Sleepy" {
			continue
		}
		found = true
		if st.Took < 10*time.Millisecond {
			t.Fatalf("Sleepy stage Took=%s, want >= 10ms", st.Took)
		}
	}
	if !found {
		t.Fatalf("Sleepy stage not surfaced in trace: %+v", exec.Stages)
	}
}

// TestRecallExplainerProjectsExecutionPerDebug exercises every quadrant
// of (IncludeLanes, IncludeStages) to lock down the projection rules
// runRecall enforces between the always-on internal trace and the
// caller-visible Execution.
func TestRecallExplainerProjectsExecutionPerDebug(t *testing.T) {
	ctx := context.Background()
	m, err := New(memidx.New(), WithRequireUserID())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := Scope{RuntimeID: "r1", UserID: "u1"}
	if _, err := m.Add(ctx, scope, Entry{
		ID: "go", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	rx := m.(RecallExplainer)
	cases := []struct {
		name       string
		debug      retrieval.SearchDebug
		wantNil    bool
		wantLanes  bool
		wantStages bool
	}{
		{"zero debug → nil execution", retrieval.SearchDebug{}, true, false, false},
		{"lanes only", retrieval.SearchDebug{IncludeLanes: true}, false, true, false},
		{"stages only", retrieval.SearchDebug{IncludeStages: true}, false, false, true},
		{"both", retrieval.SearchDebug{IncludeLanes: true, IncludeStages: true}, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, exec, err := rx.RecallExplain(ctx, scope, Request{Query: "Go", TopK: 5, Debug: tc.debug})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNil {
				if exec != nil {
					t.Fatalf("expected nil execution, got %+v", exec)
				}
				return
			}
			if exec == nil {
				t.Fatal("expected non-nil execution")
			}
			if tc.wantLanes && len(exec.Lanes) == 0 {
				t.Fatal("expected lanes")
			}
			if !tc.wantLanes && len(exec.Lanes) != 0 {
				t.Fatalf("expected no lanes, got %+v", exec.Lanes)
			}
			if tc.wantStages && len(exec.Stages) == 0 {
				t.Fatal("expected stages")
			}
			if !tc.wantStages && len(exec.Stages) != 0 {
				t.Fatalf("expected no stages, got %+v", exec.Stages)
			}
		})
	}
}

// sleepyStage is a no-op pipeline stage that only sleeps; used to give
// the stage trace a measurable, deterministic duration.
type sleepyStage struct {
	name string
	dur  time.Duration
}

func (s sleepyStage) Name() string { return s.name }
func (s sleepyStage) Run(ctx context.Context, _ *pipeline.State) error {
	select {
	case <-time.After(s.dur):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestRecallDoesNotLeakExecution ensures Memory.Recall keeps its narrow
// signature — callers that don't opt into RecallExplainer must not be
// able to observe the explanation through the public Recall path even
// when Debug is set.
func TestRecallDoesNotLeakExecution(t *testing.T) {
	ctx := context.Background()
	m, err := New(memidx.New(), WithRequireUserID())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := Scope{RuntimeID: "r1", UserID: "u1"}
	if _, err := m.Add(ctx, scope, Entry{
		ID: "go", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := m.Recall(ctx, scope, Request{
		Query: "Go", TopK: 5,
		Debug: retrieval.SearchDebug{IncludeLanes: true, IncludeStages: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit even when Debug is set")
	}
}
