package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// stubEngine records each Execute call so tests can assert that
// ResumeFrom / ResumeContext arrive populated.
type stubEngine struct {
	gotRun     engine.Run
	gotResume  engine.ResumeContext
	gotResOK   bool
	resumeErr  error
	canResume  func(engine.Checkpoint) error
	executions int
}

func (s *stubEngine) Execute(ctx context.Context, run engine.Run, _ engine.Host, board *engine.Board) (*engine.Board, error) {
	s.executions++
	s.gotRun = run
	s.gotResume, s.gotResOK = engine.ResumeContextFromContext(ctx)
	return board, s.resumeErr
}

// resumerEngine bolts CanResume onto stubEngine to cover the optional
// Resumer interface.
type resumerEngine struct {
	stubEngine
}

func (r *resumerEngine) CanResume(cp engine.Checkpoint) error {
	if r.canResume != nil {
		return r.canResume(cp)
	}
	return nil
}

// memStore is a minimal in-memory CheckpointStore for tests.
type memStore struct {
	cp *engine.Checkpoint
}

func (m *memStore) Save(_ context.Context, cp engine.Checkpoint) error {
	cp2 := cp
	m.cp = &cp2
	return nil
}

func (m *memStore) Load(_ context.Context, _ string) (*engine.Checkpoint, error) {
	return m.cp, nil
}

func TestIsResumable(t *testing.T) {
	if engine.IsResumable(&stubEngine{}) {
		t.Fatal("stubEngine must not satisfy Resumer")
	}
	if !engine.IsResumable(&resumerEngine{}) {
		t.Fatal("resumerEngine must satisfy Resumer")
	}
}

func TestLoadAndResume_FreshStart(t *testing.T) {
	eng := &stubEngine{}
	host := engine.NoopHost{}
	store := &memStore{}

	_, err := engine.LoadAndResume(context.Background(), eng, host, store,
		engine.Run{ID: "r1"}, nil)
	if err != nil {
		t.Fatalf("LoadAndResume: %v", err)
	}
	if eng.executions != 1 {
		t.Fatalf("want 1 execution, got %d", eng.executions)
	}
	if eng.gotRun.ResumeFrom != nil {
		t.Fatal("fresh start must leave Run.ResumeFrom nil")
	}
	if !eng.gotResOK {
		t.Fatal("ResumeContext should be populated even on fresh starts")
	}
	if eng.gotResume.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1", eng.gotResume.Attempt)
	}
	if eng.gotResume.Signal != "manual" {
		t.Fatalf("default Signal = %q, want manual", eng.gotResume.Signal)
	}
}

func TestLoadAndResume_RequiresCheckpointWhenFreshDisallowed(t *testing.T) {
	eng := &stubEngine{}
	store := &memStore{}

	_, err := engine.LoadAndResume(context.Background(), eng, engine.NoopHost{}, store,
		engine.Run{ID: "r1"}, nil,
		engine.WithFreshStartAllowed(false),
	)
	if err == nil {
		t.Fatal("expected NotFound error")
	}
	if !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
	if eng.executions != 0 {
		t.Fatal("must not call Execute when no checkpoint and fresh disallowed")
	}
}

func TestLoadAndResume_ResumePathPopulatesContext(t *testing.T) {
	eng := &stubEngine{}
	cpAt := time.Now().Add(-time.Hour)
	store := &memStore{cp: &engine.Checkpoint{
		ExecID:    "r1",
		Step:      "node-3",
		Timestamp: cpAt,
	}}

	_, err := engine.LoadAndResume(context.Background(), eng, engine.NoopHost{}, store,
		engine.Run{ID: "r1"}, nil,
		engine.WithResumeSignal("crash"),
	)
	if err != nil {
		t.Fatalf("LoadAndResume: %v", err)
	}
	if eng.gotRun.ResumeFrom == nil || eng.gotRun.ResumeFrom.Step != "node-3" {
		t.Fatalf("ResumeFrom not propagated: %+v", eng.gotRun.ResumeFrom)
	}
	if !eng.gotResOK || eng.gotResume.Attempt < 2 {
		t.Fatalf("resume Attempt should be >= 2, got %d (ok=%v)", eng.gotResume.Attempt, eng.gotResOK)
	}
	if eng.gotResume.Signal != "crash" {
		t.Fatalf("Signal = %q, want crash", eng.gotResume.Signal)
	}
	if !eng.gotResume.CheckpointAt.Equal(cpAt) {
		t.Fatalf("CheckpointAt = %v, want %v", eng.gotResume.CheckpointAt, cpAt)
	}
}

func TestLoadAndResume_RejectsExecIDMismatch(t *testing.T) {
	eng := &stubEngine{}
	store := &memStore{cp: &engine.Checkpoint{ExecID: "other"}}

	_, err := engine.LoadAndResume(context.Background(), eng, engine.NoopHost{}, store,
		engine.Run{ID: "r1"}, nil)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("want Validation error, got %v", err)
	}
	if eng.executions != 0 {
		t.Fatal("must not call Execute on exec_id mismatch")
	}
}

func TestLoadAndResume_HonoursResumerCanResume(t *testing.T) {
	wantErr := errdefs.NotAvailable(errors.New("incompatible engine version"))
	eng := &resumerEngine{}
	eng.canResume = func(_ engine.Checkpoint) error { return wantErr }

	store := &memStore{cp: &engine.Checkpoint{ExecID: "r1"}}

	_, err := engine.LoadAndResume(context.Background(), eng, engine.NoopHost{}, store,
		engine.Run{ID: "r1"}, nil)
	if err == nil {
		t.Fatal("expected CanResume error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wraps %v", err, wantErr)
	}
	if eng.executions != 0 {
		t.Fatal("must not Execute when CanResume rejects")
	}
}

func TestResumeContextFromContext_NilCtxReturnsFalse(t *testing.T) {
	if _, ok := engine.ResumeContextFromContext(nil); ok {
		t.Fatal("nil ctx must return ok=false")
	}
}
