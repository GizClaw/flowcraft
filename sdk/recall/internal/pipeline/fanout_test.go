package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// fakeProjection is a minimal port.Projection that records every call
// and can be configured to fail on a chosen op (project/forget/rebuild)
// or to honour ctx.Err.
type fakeProjection struct {
	name       string
	level      port.Consistency
	projectErr error
	forgetErr  error
	rebuildErr error

	projectCalls int
	forgetCalls  int
	rebuildCalls int
}

func (p *fakeProjection) Name() string                  { return p.name }
func (p *fakeProjection) Consistency() port.Consistency { return p.level }

func (p *fakeProjection) Project(_ context.Context, _ []domain.TemporalFact) error {
	p.projectCalls++
	return p.projectErr
}

func (p *fakeProjection) Forget(_ context.Context, _ domain.Scope, _ []string) error {
	p.forgetCalls++
	return p.forgetErr
}

func (p *fakeProjection) Rebuild(_ context.Context, _ domain.Scope, _ []domain.TemporalFact) error {
	p.rebuildCalls++
	return p.rebuildErr
}

func sampleFacts() []domain.TemporalFact {
	return []domain.TemporalFact{{ID: "f1"}}
}

func sampleScope() domain.Scope { return domain.Scope{RuntimeID: "rt", UserID: "u"} }

func TestNewFanout_PartitionsAndNilHook(t *testing.T) {
	req := &fakeProjection{name: "req", level: port.Required}
	opt := &fakeProjection{name: "opt", level: port.Optional}
	// nil entries must be skipped; nil hook must fall back to NopHook.
	f := NewFanout([]port.Projection{nil, req, opt}, nil)

	if got := f.RequiredNames(); len(got) != 1 || got[0] != "req" {
		t.Errorf("RequiredNames = %v, want [req]", got)
	}
	if hook := f.Telemetry(); hook == nil {
		t.Error("Telemetry must never return nil — nil hook should map to NopHook")
	}
}

func TestFanout_NilReceiver_AllNoOps(t *testing.T) {
	var f *Fanout
	if err := f.ProjectRequired(context.Background(), sampleFacts()); err != nil {
		t.Errorf("nil.ProjectRequired = %v, want nil", err)
	}
	f.ProjectOptional(context.Background(), sampleFacts()) // must not panic
	if err := f.ForgetRequired(context.Background(), sampleScope(), []string{"f1"}); err != nil {
		t.Errorf("nil.ForgetRequired = %v", err)
	}
	f.ForgetOptional(context.Background(), sampleScope(), []string{"f1"})
	if err := f.RebuildRequired(context.Background(), sampleScope(), sampleFacts()); err != nil {
		t.Errorf("nil.RebuildRequired = %v", err)
	}
	f.RebuildOptional(context.Background(), sampleScope(), sampleFacts())
	if got := f.RequiredNames(); got != nil {
		t.Errorf("nil.RequiredNames = %v, want nil", got)
	}
	if hook := f.Telemetry(); hook == nil {
		t.Error("nil.Telemetry must still return NopHook")
	}
}

// TestProjectRequired_ShortCircuitWrapsName is the headline guarantee
// of the required tier: on first failure the remaining required
// projections must NOT run, and the returned error must name the
// failing projection + op for caller-side compensation routing.
func TestProjectRequired_ShortCircuitWrapsName(t *testing.T) {
	boom := errors.New("boom")
	r1 := &fakeProjection{name: "r1", level: port.Required, projectErr: boom}
	r2 := &fakeProjection{name: "r2", level: port.Required}
	f := NewFanout([]port.Projection{r1, r2}, nil)

	err := f.ProjectRequired(context.Background(), sampleFacts())
	if !errors.Is(err, boom) {
		t.Fatalf("error must wrap original via %%w: %v", err)
	}
	if msg := err.Error(); !contains(msg, "\"r1\"") || !contains(msg, "project") {
		t.Errorf("error must name failing projection + op, got %q", msg)
	}
	if r1.projectCalls != 1 || r2.projectCalls != 0 {
		t.Errorf("short-circuit broken: r1=%d r2=%d", r1.projectCalls, r2.projectCalls)
	}
}

func TestProjectRequired_SkipsOnEmptyFactsOrNoRequired(t *testing.T) {
	p := &fakeProjection{name: "r1", level: port.Required, projectErr: errors.New("never")}
	f := NewFanout([]port.Projection{p}, nil)

	if err := f.ProjectRequired(context.Background(), nil); err != nil {
		t.Errorf("empty facts must skip, got %v", err)
	}
	if p.projectCalls != 0 {
		t.Errorf("must not call projection with empty facts")
	}

	// Optional-only fanout must also short-circuit without invoking
	// the required loop.
	opt := &fakeProjection{name: "o1", level: port.Optional}
	f2 := NewFanout([]port.Projection{opt}, nil)
	if err := f2.ProjectRequired(context.Background(), sampleFacts()); err != nil {
		t.Errorf("no required projections must skip, got %v", err)
	}
}

func TestProjectRequired_HonoursContext(t *testing.T) {
	p := &fakeProjection{name: "r1", level: port.Required}
	f := NewFanout([]port.Projection{p}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.ProjectRequired(ctx, sampleFacts()); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx must return context.Canceled, got %v", err)
	}
	if p.projectCalls != 0 {
		t.Errorf("must not invoke projection when ctx already cancelled")
	}
}

// TestProjectOptional_FailuresSwallowed pins the best-effort contract:
// optional failures must never escape, and ALL optional projections
// must still get a chance to run (no short-circuit).
func TestProjectOptional_FailuresSwallowed(t *testing.T) {
	o1 := &fakeProjection{name: "o1", level: port.Optional, projectErr: errors.New("boom")}
	o2 := &fakeProjection{name: "o2", level: port.Optional}
	f := NewFanout([]port.Projection{o1, o2}, nil)
	f.ProjectOptional(context.Background(), sampleFacts())
	if o1.projectCalls != 1 || o2.projectCalls != 1 {
		t.Errorf("optional must not short-circuit: o1=%d o2=%d", o1.projectCalls, o2.projectCalls)
	}
}

func TestForgetAndRebuild(t *testing.T) {
	r := &fakeProjection{name: "r", level: port.Required}
	o := &fakeProjection{name: "o", level: port.Optional}
	f := NewFanout([]port.Projection{r, o}, nil)

	if err := f.Forget(context.Background(), sampleScope(), []string{"f1"}); err != nil {
		t.Fatalf("Forget = %v", err)
	}
	if r.forgetCalls != 1 || o.forgetCalls != 1 {
		t.Errorf("Forget must hit required + optional, got r=%d o=%d", r.forgetCalls, o.forgetCalls)
	}

	if err := f.Rebuild(context.Background(), sampleScope(), sampleFacts()); err != nil {
		t.Fatalf("Rebuild = %v", err)
	}
	if r.rebuildCalls != 1 || o.rebuildCalls != 1 {
		t.Errorf("Rebuild must hit required + optional, got r=%d o=%d", r.rebuildCalls, o.rebuildCalls)
	}

	// RebuildRequired must run with empty facts (rebuild empties projections).
	if err := f.RebuildRequired(context.Background(), sampleScope(), nil); err != nil {
		t.Errorf("RebuildRequired with empty facts = %v", err)
	}
	if r.rebuildCalls != 2 {
		t.Errorf("RebuildRequired must not skip on empty facts: r.rebuildCalls=%d", r.rebuildCalls)
	}
}

func TestForgetRequired_SkipsOnEmptyFactIDs(t *testing.T) {
	r := &fakeProjection{name: "r", level: port.Required, forgetErr: errors.New("never")}
	f := NewFanout([]port.Projection{r}, nil)
	if err := f.ForgetRequired(context.Background(), sampleScope(), nil); err != nil {
		t.Errorf("empty factIDs must skip, got %v", err)
	}
	if r.forgetCalls != 0 {
		t.Errorf("must not invoke projection with empty factIDs")
	}
}

// recordingHook captures every OnStage call so we can confirm the
// fanout publishes its constructor-supplied hook through Telemetry().
type recordingHook struct{ events []diagnostic.StageDiagnostic }

func (h *recordingHook) OnStage(e diagnostic.StageDiagnostic) { h.events = append(h.events, e) }

func TestTelemetry_RoundTripsConstructorHook(t *testing.T) {
	hook := &recordingHook{}
	f := NewFanout(nil, hook)
	got := f.Telemetry()
	if got != hook {
		t.Errorf("Telemetry must return the constructor-supplied hook (got %T)", got)
	}
}

func TestErrProjectionDisabled_IsStableSentinel(t *testing.T) {
	wrapped := errors.Join(ErrProjectionDisabled, errors.New("ctx"))
	if !errors.Is(wrapped, ErrProjectionDisabled) {
		t.Error("ErrProjectionDisabled must remain matchable via errors.Is after wrapping")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
