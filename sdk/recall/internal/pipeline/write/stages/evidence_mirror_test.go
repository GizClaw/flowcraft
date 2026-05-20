package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

type fakeEvidence struct {
	appends []string
	err     error
}

func (f *fakeEvidence) Append(_ context.Context, _ domain.Scope, factID string, _ []domain.EvidenceRef) error {
	f.appends = append(f.appends, factID)
	return f.err
}
func (*fakeEvidence) Get(context.Context, domain.Scope, string) (domain.EvidenceRef, error) {
	return domain.EvidenceRef{}, nil
}
func (*fakeEvidence) ListByFact(context.Context, domain.Scope, string) ([]domain.EvidenceRef, error) {
	return nil, nil
}
func (*fakeEvidence) ListFactIDs(context.Context, domain.Scope) ([]string, error) { return nil, nil }
func (*fakeEvidence) ForgetByFact(context.Context, domain.Scope, []string) error  { return nil }
func (*fakeEvidence) Close() error                                                { return nil }

func TestEvidenceMirror_NilStoreIsNoop(t *testing.T) {
	s := stages.NewEvidenceMirror(nil, nil)
	state := &write.WriteState{Resolution: domain.Resolution{Facts: []domain.TemporalFact{{ID: "a"}}}}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, ok := d.(diagnostic.EvidenceMirrorDetail); !ok || got.EventsRecorded != 0 {
		t.Errorf("Detail = %#v", d)
	}
}

func TestEvidenceMirror_RecordsRefs(t *testing.T) {
	ev := &fakeEvidence{}
	s := stages.NewEvidenceMirror(ev, nil)
	state := &write.WriteState{Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
		ID:           "a",
		EvidenceRefs: []domain.EvidenceRef{{ID: "r1"}, {ID: "r2"}},
	}}}}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.(diagnostic.EvidenceMirrorDetail).EventsRecorded; got != 2 {
		t.Errorf("EventsRecorded = %d", got)
	}
	if state.EvidenceMirrored != 2 {
		t.Errorf("state.EvidenceMirrored = %d", state.EvidenceMirrored)
	}
}

func TestEvidenceMirror_FailureIsNonFatal(t *testing.T) {
	boom := errors.New("mirror boom")
	ev := &fakeEvidence{err: boom}
	hook := &recordHook{}
	s := stages.NewEvidenceMirror(ev, hook)
	state := &write.WriteState{Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
		ID: "a", EvidenceRefs: []domain.EvidenceRef{{ID: "r1"}},
	}}}}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("evidence_mirror MUST swallow err: %v", err)
	}
	if state.EvidenceMirrorErr == nil {
		t.Error("EvidenceMirrorErr should be set so the legacy bridge can surface the error")
	}
	if len(hook.projections) != 1 || hook.projections[0].Projection != "evidence" {
		t.Errorf("legacy OnProjection not emitted: %+v", hook.projections)
	}
	_ = d
}

// silence unused import port — port is used transitively by fakeEvidence.
var _ = port.OpProject
