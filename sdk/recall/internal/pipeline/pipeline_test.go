package pipeline_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// testState is the dummy state pointer the test stages thread.
// Each stage records its observed side-effects so the assertions
// can verify ordering, compensator invocation, and detach-ctx
// behaviour without dragging in real Read/Write/Rebuild state.
type testState struct {
	runOrder        []string
	compensated     []string
	skipObserved    []string
	ctxAtCompensate []bool // true when ctx was still alive at compensate time
	stages          []diagnostic.StageDiagnostic
}

// fakeStage records its Name into runOrder and returns the supplied
// detail.
type fakeStage struct {
	name   string
	detail diagnostic.StageDetail
}

func (s fakeStage) Name() string { return s.name }
func (s fakeStage) Run(_ context.Context, st *testState) (diagnostic.StageDetail, error) {
	st.runOrder = append(st.runOrder, s.name)
	return s.detail, nil
}

// failingStage returns an error so the framework triggers
// compensation.
type failingStage struct {
	name string
	err  error
}

func (s failingStage) Name() string { return s.name }
func (s failingStage) Run(_ context.Context, st *testState) (diagnostic.StageDetail, error) {
	st.runOrder = append(st.runOrder, s.name)
	return nil, s.err
}

// shortCircuitStage returns ShortCircuit so the framework stops.
type shortCircuitStage struct {
	name   string
	reason string
}

func (s shortCircuitStage) Name() string { return s.name }
func (s shortCircuitStage) Run(_ context.Context, st *testState) (diagnostic.StageDetail, error) {
	st.runOrder = append(st.runOrder, s.name)
	return nil, pipeline.ShortCircuitWith(s.reason)
}

// compensatingStage records both Run and Compensate so the test
// can assert reverse-order rollback. The compensator also inspects
// ctx.Err so we can prove the framework supplied a detached ctx
// even if the parent was cancelled.
type compensatingStage struct {
	name string
}

func (s compensatingStage) Name() string { return s.name }
func (s compensatingStage) Run(_ context.Context, st *testState) (diagnostic.StageDetail, error) {
	st.runOrder = append(st.runOrder, s.name)
	return nil, nil
}
func (s compensatingStage) Compensate(ctx context.Context, st *testState) error {
	st.compensated = append(st.compensated, s.name)
	st.ctxAtCompensate = append(st.ctxAtCompensate, ctx.Err() == nil)
	return nil
}

// conditionalSkipStage reports Skip=true and supplies a Detail the
// test asserts is propagated into the Status=Skipped diagnostic.
type conditionalSkipStage struct {
	name   string
	detail diagnostic.StageDetail
}

func (s conditionalSkipStage) Name() string { return s.name }
func (s conditionalSkipStage) Run(_ context.Context, st *testState) (diagnostic.StageDetail, error) {
	st.runOrder = append(st.runOrder, s.name)
	return nil, nil
}
func (s conditionalSkipStage) Skip(_ context.Context, st *testState) (bool, diagnostic.StageDetail) {
	st.skipObserved = append(st.skipObserved, s.name)
	return true, s.detail
}

// captureHook records every OnStage call so test 5 can verify the
// framework emitted exactly one event per trace entry.
type captureHook struct {
	port.TelemetryHook
	events []diagnostic.StageDiagnostic
}

func (h *captureHook) OnStage(d diagnostic.StageDiagnostic) {
	h.events = append(h.events, d)
}

func newCaptureHook() *captureHook {
	return &captureHook{}
}

// appendTrace is the standard TraceAppender for tests: it pushes
// every diagnostic into the testState's stages slice in emission
// order.
func appendTrace(st *testState, d diagnostic.StageDiagnostic) {
	st.stages = append(st.stages, d)
}

// newPipeline is the tiny helper that wires the trace appender and
// hook. Each test supplies its own stage list.
func newPipeline(stages []pipeline.Stage[*testState], hook port.TelemetryHook) *pipeline.Pipeline[*testState] {
	return pipeline.NewPipeline[*testState](diagnostic.PhaseWrite, stages, hook, appendTrace)
}

func TestPipeline_HappyPath(t *testing.T) {
	hook := newCaptureHook()
	st := &testState{}
	p := newPipeline([]pipeline.Stage[*testState]{
		fakeStage{name: "s1"},
		fakeStage{name: "s2"},
		fakeStage{name: "s3"},
	}, hook)

	before := time.Now()
	if err := p.Run(context.Background(), st); err != nil {
		t.Fatalf("Run returned err: %v", err)
	}
	after := time.Now()

	if got := st.runOrder; len(got) != 3 || got[0] != "s1" || got[1] != "s2" || got[2] != "s3" {
		t.Fatalf("runOrder = %v, want [s1 s2 s3]", got)
	}
	if len(st.stages) != 3 {
		t.Fatalf("trace.Stages len = %d, want 3", len(st.stages))
	}
	for i, d := range st.stages {
		if d.Status != diagnostic.StatusOK {
			t.Errorf("stage %d Status = %q, want ok", i, d.Status)
		}
		if d.StartAt.Before(before) || d.StartAt.After(after) {
			t.Errorf("stage %d StartAt %v outside [%v, %v]", i, d.StartAt, before, after)
		}
		if d.Phase != diagnostic.PhaseWrite {
			t.Errorf("stage %d Phase = %q, want write", i, d.Phase)
		}
		if d.Order != i {
			t.Errorf("stage %d Order = %d, want %d", i, d.Order, i)
		}
	}
	for i := 1; i < len(st.stages); i++ {
		if st.stages[i].StartAt.Before(st.stages[i-1].StartAt) {
			t.Errorf("StartAt non-monotonic at %d: %v before %v", i, st.stages[i].StartAt, st.stages[i-1].StartAt)
		}
	}
	if len(hook.events) != len(st.stages) {
		t.Fatalf("hook events = %d, trace = %d", len(hook.events), len(st.stages))
	}
}

func TestPipeline_ShortCircuitStopsAndSkipsRemaining(t *testing.T) {
	hook := newCaptureHook()
	st := &testState{}
	p := newPipeline([]pipeline.Stage[*testState]{
		fakeStage{name: "s1"},
		shortCircuitStage{name: "s2", reason: "no_intent"},
		// s3 must not run and must not appear in trace.
		compensatingStage{name: "s3"},
	}, hook)

	if err := p.Run(context.Background(), st); err != nil {
		t.Fatalf("Run returned err: %v", err)
	}

	if got := st.runOrder; len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Fatalf("runOrder = %v, want [s1 s2]", got)
	}
	if len(st.stages) != 2 {
		t.Fatalf("trace.Stages len = %d, want 2 (s3 must not be in trace)", len(st.stages))
	}
	if st.stages[0].Status != diagnostic.StatusOK {
		t.Errorf("s1 Status = %q, want ok", st.stages[0].Status)
	}
	if st.stages[1].Status != diagnostic.StatusShortCircuit {
		t.Errorf("s2 Status = %q, want short_circuit", st.stages[1].Status)
	}
	if st.stages[1].Err != "no_intent" {
		t.Errorf("s2 Err = %q, want %q", st.stages[1].Err, "no_intent")
	}
	if len(st.compensated) != 0 {
		t.Errorf("compensation unexpectedly ran on ShortCircuit: %v", st.compensated)
	}
	if len(hook.events) != 2 {
		t.Fatalf("hook events = %d, want 2", len(hook.events))
	}
}

func TestPipeline_FailureTriggersReverseCompensateWithDetachedCtx(t *testing.T) {
	hook := newCaptureHook()
	st := &testState{}
	boom := errors.New("project_required: store down")
	p := newPipeline([]pipeline.Stage[*testState]{
		compensatingStage{name: "s1"},
		compensatingStage{name: "s2"},
		failingStage{name: "s3", err: boom},
	}, hook)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the parent ctx BEFORE Run completes — failingStage
	// would normally observe ctx.Err here but our stages don't
	// inspect it. The point is that Compensate (run AFTER Run
	// returns) must still see ctx.Err()==nil because the framework
	// detached cancellation.
	cancel()

	err := p.Run(ctx, st)
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want %v", err, boom)
	}

	if got := st.runOrder; len(got) != 3 || got[0] != "s1" || got[1] != "s2" || got[2] != "s3" {
		t.Fatalf("runOrder = %v, want [s1 s2 s3]", got)
	}
	if got := st.compensated; len(got) != 2 || got[0] != "s2" || got[1] != "s1" {
		t.Fatalf("compensated = %v, want [s2 s1] (reverse)", got)
	}
	for i, alive := range st.ctxAtCompensate {
		if !alive {
			t.Errorf("compensate #%d observed ctx.Err != nil; detach-ctx broken", i)
		}
	}

	// Trace: s1+s2 emitted as ok, s3 as failed, then s2+s1 re-
	// emitted as compensated (reverse order). Total 5 entries.
	if len(st.stages) != 5 {
		t.Fatalf("trace.Stages len = %d, want 5: %v", len(st.stages), st.stages)
	}
	wantStatus := []diagnostic.Status{
		diagnostic.StatusOK,
		diagnostic.StatusOK,
		diagnostic.StatusFailed,
		diagnostic.StatusCompensated,
		diagnostic.StatusCompensated,
	}
	wantStage := []string{"s1", "s2", "s3", "s2", "s1"}
	for i, want := range wantStatus {
		if st.stages[i].Status != want {
			t.Errorf("stage[%d] Status = %q, want %q", i, st.stages[i].Status, want)
		}
		if st.stages[i].Stage != wantStage[i] {
			t.Errorf("stage[%d] Stage = %q, want %q", i, st.stages[i].Stage, wantStage[i])
		}
	}
	if st.stages[2].Err != boom.Error() {
		t.Errorf("failed stage Err = %q, want %q", st.stages[2].Err, boom.Error())
	}
	if len(hook.events) != 5 {
		t.Fatalf("hook events = %d, want 5", len(hook.events))
	}
}

func TestPipeline_ConditionalSkipPropagatesDetail(t *testing.T) {
	hook := newCaptureHook()
	st := &testState{}
	skipDetail := diagnostic.FederationMergeDetail{InputCount: 1}
	p := newPipeline([]pipeline.Stage[*testState]{
		fakeStage{name: "s1"},
		conditionalSkipStage{name: "s2", detail: skipDetail},
		fakeStage{name: "s3"},
	}, hook)

	if err := p.Run(context.Background(), st); err != nil {
		t.Fatalf("Run err: %v", err)
	}

	if got := st.runOrder; len(got) != 2 || got[0] != "s1" || got[1] != "s3" {
		t.Fatalf("runOrder = %v, want [s1 s3] (s2 must not Run)", got)
	}
	if len(st.skipObserved) != 1 || st.skipObserved[0] != "s2" {
		t.Fatalf("skipObserved = %v, want [s2]", st.skipObserved)
	}
	if len(st.stages) != 3 {
		t.Fatalf("trace.Stages len = %d, want 3", len(st.stages))
	}
	if st.stages[0].Status != diagnostic.StatusOK {
		t.Errorf("stage[0] Status = %q, want ok", st.stages[0].Status)
	}
	if st.stages[1].Status != diagnostic.StatusSkipped {
		t.Errorf("stage[1] Status = %q, want skipped", st.stages[1].Status)
	}
	if st.stages[1].Stage != "s2" {
		t.Errorf("stage[1] Stage = %q, want s2", st.stages[1].Stage)
	}
	gotDetail, ok := st.stages[1].Detail.(diagnostic.FederationMergeDetail)
	if !ok || gotDetail != skipDetail {
		t.Errorf("stage[1] Detail = %#v, want %#v", st.stages[1].Detail, skipDetail)
	}
	if st.stages[2].Status != diagnostic.StatusOK || st.stages[2].Stage != "s3" {
		t.Errorf("stage[2] = %+v, want s3/ok", st.stages[2])
	}
	if len(hook.events) != 3 {
		t.Fatalf("hook events = %d, want 3", len(hook.events))
	}
}

func TestPipeline_OnStageMatchesTrace(t *testing.T) {
	hook := newCaptureHook()
	st := &testState{}
	p := newPipeline([]pipeline.Stage[*testState]{
		fakeStage{name: "s1"},
		conditionalSkipStage{name: "s2", detail: diagnostic.IntentDetail{RawQuery: "skipped"}},
		fakeStage{name: "s3"},
	}, hook)

	if err := p.Run(context.Background(), st); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if len(hook.events) != len(st.stages) {
		t.Fatalf("hook events = %d, trace = %d", len(hook.events), len(st.stages))
	}
	for i := range hook.events {
		if hook.events[i].Stage != st.stages[i].Stage || hook.events[i].Status != st.stages[i].Status {
			t.Errorf("hook[%d] = %+v, trace[%d] = %+v", i, hook.events[i], i, st.stages[i])
		}
	}
}

func TestPipeline_EmptyStagesIsNoOp(t *testing.T) {
	hook := newCaptureHook()
	st := &testState{}
	p := newPipeline(nil, hook)
	if err := p.Run(context.Background(), st); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if len(st.stages) != 0 || len(hook.events) != 0 {
		t.Fatalf("expected zero emission for empty pipeline; trace=%d hook=%d", len(st.stages), len(hook.events))
	}
}

func TestPipeline_IsShortCircuit(t *testing.T) {
	if !pipeline.IsShortCircuit(pipeline.ShortCircuitWith("reason")) {
		t.Error("IsShortCircuit(sentinel) = false")
	}
	if pipeline.IsShortCircuit(errors.New("regular")) {
		t.Error("IsShortCircuit(regular err) = true")
	}
	if pipeline.IsShortCircuit(nil) {
		t.Error("IsShortCircuit(nil) = true")
	}
}
