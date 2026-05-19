package projection

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

type stubProj struct {
	name        string
	consistency Consistency
	projectErr  error
	forgetErr   error
	rebuildErr  error
	projectN    int
	forgetN     int
	rebuildN    int
}

func (s *stubProj) Name() string             { return s.name }
func (s *stubProj) Consistency() Consistency { return s.consistency }
func (s *stubProj) Project(_ context.Context, facts []model.TemporalFact) error {
	s.projectN++
	return s.projectErr
}
func (s *stubProj) Forget(_ context.Context, _ model.Scope, ids []string) error {
	s.forgetN++
	return s.forgetErr
}
func (s *stubProj) Rebuild(_ context.Context, _ model.Scope, facts []model.TemporalFact) error {
	s.rebuildN++
	return s.rebuildErr
}

type recordingHook struct {
	events []ProjectionEvent
	drifts []DriftEvent
}

func (r *recordingHook) OnProjection(e ProjectionEvent) { r.events = append(r.events, e) }
func (r *recordingHook) OnDrift(e DriftEvent)           { r.drifts = append(r.drifts, e) }

func TestFanout_RequiredFailureAborts(t *testing.T) {
	failing := &stubProj{name: "retrieval", consistency: Required, projectErr: errors.New("boom")}
	ok := &stubProj{name: "entity", consistency: Required}
	opt := &stubProj{name: "profile", consistency: Optional}
	hook := &recordingHook{}
	f := New([]Projection{failing, ok, opt}, hook)

	err := f.Project(context.Background(), []model.TemporalFact{{ID: "x"}})
	if err == nil {
		t.Fatal("want error from required projection failure")
	}
	if ok.projectN != 0 {
		t.Errorf("second required projection should not run on first failure, got %d", ok.projectN)
	}
	if opt.projectN != 0 {
		t.Errorf("optional projections should not run when required fails, got %d", opt.projectN)
	}
	if len(hook.events) != 1 || hook.events[0].Projection != "retrieval" || hook.events[0].Err == nil {
		t.Errorf("hook events = %+v", hook.events)
	}
}

func TestFanout_OptionalFailureNotFatal(t *testing.T) {
	req := &stubProj{name: "retrieval", consistency: Required}
	opt := &stubProj{name: "profile", consistency: Optional, projectErr: errors.New("optional boom")}
	hook := &recordingHook{}
	f := New([]Projection{req, opt}, hook)

	if err := f.Project(context.Background(), []model.TemporalFact{{ID: "x"}}); err != nil {
		t.Fatalf("optional failure must not abort: %v", err)
	}
	if req.projectN != 1 || opt.projectN != 1 {
		t.Fatalf("both projections must run: req=%d opt=%d", req.projectN, opt.projectN)
	}
	var sawOptionalErr bool
	for _, e := range hook.events {
		if e.Projection == "profile" && e.Err != nil {
			sawOptionalErr = true
		}
	}
	if !sawOptionalErr {
		t.Errorf("telemetry must observe optional failure: %+v", hook.events)
	}
}

func TestFanout_ForgetPropagatesRequiredFailure(t *testing.T) {
	failing := &stubProj{name: "retrieval", consistency: Required, forgetErr: errors.New("nope")}
	opt := &stubProj{name: "profile", consistency: Optional}
	f := New([]Projection{failing, opt}, NopTelemetry{})
	if err := f.Forget(context.Background(), model.Scope{RuntimeID: "rt"}, []string{"x"}); err == nil {
		t.Fatal("want error")
	}
	if opt.forgetN != 0 {
		t.Errorf("optional forget should not run when required fails")
	}
}

func TestFanout_NilFactsNoOp(t *testing.T) {
	p := &stubProj{name: "x", consistency: Required}
	f := New([]Projection{p}, nil)
	if err := f.Project(context.Background(), nil); err != nil {
		t.Errorf("nil facts must be noop: %v", err)
	}
	if p.projectN != 0 {
		t.Errorf("projection should not be invoked for empty fact batch")
	}
}
