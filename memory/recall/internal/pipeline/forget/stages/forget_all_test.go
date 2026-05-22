package stages_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/forget"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/forget/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// fakeStore implements just the TemporalStore methods the forget_all
// stage actually exercises. Methods we don't need return zero values
// so the surface stays minimal.
type fakeStore struct {
	facts             []domain.TemporalFact
	listErr           error
	markClosedCalls   []string
	markClosedErr     error
	deleteScopeCount  int
	deleteScopeErr    error
	deleteByScopeArgs []domain.Scope
	deleteIDs         [][]string
}

func (s *fakeStore) Append(context.Context, []domain.TemporalFact) error { return nil }
func (s *fakeStore) Get(context.Context, domain.Scope, string) (domain.TemporalFact, error) {
	return domain.TemporalFact{}, nil
}
func (s *fakeStore) List(_ context.Context, _ domain.Scope, _ port.ListQuery) ([]domain.TemporalFact, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]domain.TemporalFact, len(s.facts))
	copy(out, s.facts)
	return out, nil
}
func (s *fakeStore) FindByMergeKey(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}
func (s *fakeStore) FindByRevisionSource(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}
func (s *fakeStore) FindByOriginRequestID(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}

func (s *fakeStore) FindSupersededBy(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}
func (s *fakeStore) UpdateValidity(context.Context, domain.Scope, string, time.Time, string) error {
	return nil
}
func (s *fakeStore) ReopenValidity(context.Context, domain.Scope, string, string) error { return nil }
func (s *fakeStore) Delete(_ context.Context, _ domain.Scope, ids []string) error {
	s.deleteIDs = append(s.deleteIDs, append([]string(nil), ids...))
	return nil
}
func (s *fakeStore) UpdateFeedback(context.Context, domain.Scope, string, float64, float64) error {
	return nil
}
func (s *fakeStore) MarkClosed(_ context.Context, _ domain.Scope, factID string, _ bool) error {
	if s.markClosedErr != nil {
		return s.markClosedErr
	}
	s.markClosedCalls = append(s.markClosedCalls, factID)
	return nil
}
func (s *fakeStore) ListByID(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}
func (s *fakeStore) DeleteByScope(_ context.Context, scope domain.Scope) (int, error) {
	if s.deleteScopeErr != nil {
		return 0, s.deleteScopeErr
	}
	s.deleteByScopeArgs = append(s.deleteByScopeArgs, scope)
	return s.deleteScopeCount, nil
}
func (s *fakeStore) Close() error { return nil }

// fakeProjection records ClearScope / Project invocations so tests
// can assert fanout dispatch shape.
type fakeProjection struct {
	name       string
	level      port.Consistency
	projectErr error
	clearErr   error
	projectN   int
	clearN     int
}

func (p *fakeProjection) Name() string                  { return p.name }
func (p *fakeProjection) Consistency() port.Consistency { return p.level }
func (p *fakeProjection) Project(_ context.Context, facts []domain.TemporalFact) error {
	p.projectN += len(facts)
	return p.projectErr
}
func (p *fakeProjection) Forget(context.Context, domain.Scope, []string) error { return nil }
func (p *fakeProjection) Rebuild(context.Context, domain.Scope, []domain.TemporalFact) error {
	return nil
}
func (p *fakeProjection) ClearScope(context.Context, domain.Scope) error {
	if p.clearErr != nil {
		return p.clearErr
	}
	p.clearN++
	return nil
}

// fakeEvidenceStore returns a constant fact-id list so ForgetAll can
// snapshot the evidence count for EvidenceCleared.
type fakeEvidenceStore struct {
	factIDs []string
	listErr error
}

func (e *fakeEvidenceStore) Append(context.Context, domain.Scope, string, []domain.EvidenceRef) error {
	return nil
}
func (e *fakeEvidenceStore) Get(context.Context, domain.Scope, string) (domain.EvidenceRef, error) {
	return domain.EvidenceRef{}, nil
}
func (e *fakeEvidenceStore) ListByFact(context.Context, domain.Scope, string) ([]domain.EvidenceRef, error) {
	return nil, nil
}
func (e *fakeEvidenceStore) ListFactIDs(context.Context, domain.Scope) ([]string, error) {
	return e.factIDs, e.listErr
}
func (e *fakeEvidenceStore) ForgetByFact(context.Context, domain.Scope, []string) error { return nil }
func (e *fakeEvidenceStore) Close() error                                               { return nil }

func newRunner(t *testing.T, store port.TemporalStore, projs []port.Projection, ev port.EvidenceStore) *forget.Runner {
	t.Helper()
	fan := pipeline.NewFanout(projs, nil)
	return forget.NewRunner([]pipeline.Stage[*forget.State]{
		stages.NewForgetAll(store, fan, projs, ev),
	}, nil)
}

func TestForgetAll_Hard_ClearsProjectionsAndStore(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "alice"}
	store := &fakeStore{
		facts: []domain.TemporalFact{
			{ID: "f1", Scope: scope},
			{ID: "f2", Scope: scope},
		},
		deleteScopeCount: 2,
	}
	r := &fakeProjection{name: "retrieval", level: port.Required}
	o := &fakeProjection{name: "graph", level: port.Optional}
	ev := &fakeEvidenceStore{factIDs: []string{"f1"}}
	runner := newRunner(t, store, []port.Projection{r, o}, ev)

	state := &forget.State{
		Scope:           scope,
		Mode:            domain.ForgetHard,
		ConfirmScopeKey: scope.PartitionKey(),
	}
	state.EnsureTrace()
	if err := runner.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Deleted != 2 {
		t.Errorf("Deleted = %d, want 2", state.Deleted)
	}
	if r.clearN != 1 || o.clearN != 1 {
		t.Errorf("ClearScope calls: r=%d o=%d, want 1/1", r.clearN, o.clearN)
	}
	if len(store.deleteByScopeArgs) != 1 {
		t.Errorf("DeleteByScope calls = %d, want 1", len(store.deleteByScopeArgs))
	}
	if len(state.Trace.Stages) != 1 {
		t.Fatalf("trace stages = %d, want 1", len(state.Trace.Stages))
	}
	d, ok := state.Trace.Stages[0].Detail.(diagnostic.ForgetAllDetail)
	if !ok {
		t.Fatalf("detail type = %T, want ForgetAllDetail", state.Trace.Stages[0].Detail)
	}
	if d.Mode != "hard" || d.Deleted != 2 || d.ProjectionsCleared != 2 || d.EvidenceCleared != 1 {
		t.Errorf("detail mismatch: %+v", d)
	}
}

func TestForgetAll_Hard_ScopeKeyMismatch(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "alice"}
	store := &fakeStore{}
	runner := newRunner(t, store, nil, nil)

	state := &forget.State{
		Scope:           scope,
		Mode:            domain.ForgetHard,
		ConfirmScopeKey: "wrong",
	}
	err := runner.Run(context.Background(), state)
	if !errors.Is(err, stages.ErrScopeKeyMismatch) {
		t.Fatalf("err = %v, want ErrScopeKeyMismatch", err)
	}
	if !errdefs.IsForbidden(err) {
		t.Fatalf("ErrScopeKeyMismatch must map to Forbidden: %v", err)
	}
	if state.Deleted != 0 {
		t.Errorf("Deleted = %d on guard fail, want 0", state.Deleted)
	}
}

func TestForgetAll_Soft_MarksClosedAndReprojects(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "alice"}
	store := &fakeStore{
		facts: []domain.TemporalFact{
			{ID: "f1", Scope: scope},
			{ID: "f2", Scope: scope},
		},
	}
	r := &fakeProjection{name: "retrieval", level: port.Required}
	runner := newRunner(t, store, []port.Projection{r}, nil)

	state := &forget.State{Scope: scope, Mode: domain.ForgetSoft}
	state.EnsureTrace()
	if err := runner.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Deleted != 2 {
		t.Errorf("Deleted = %d, want 2", state.Deleted)
	}
	if len(store.markClosedCalls) != 2 {
		t.Errorf("MarkClosed calls = %d, want 2", len(store.markClosedCalls))
	}
	if r.projectN != 2 {
		t.Errorf("Project re-applied count = %d, want 2", r.projectN)
	}
	if r.clearN != 0 {
		t.Errorf("Soft must NOT call ClearScope; got %d", r.clearN)
	}
	d := state.Trace.Stages[0].Detail.(diagnostic.ForgetAllDetail)
	if d.Mode != "soft" || d.Deleted != 2 || d.ProjectionsCleared != 0 || d.EvidenceCleared != 0 {
		t.Errorf("detail mismatch: %+v", d)
	}
}

func TestForgetAll_EmptyScope_NoOps(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "alice"}
	store := &fakeStore{facts: nil}
	r := &fakeProjection{name: "retrieval", level: port.Required}
	runner := newRunner(t, store, []port.Projection{r}, nil)

	state := &forget.State{
		Scope:           scope,
		Mode:            domain.ForgetHard,
		ConfirmScopeKey: scope.PartitionKey(),
	}
	if err := runner.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Deleted != 0 || r.clearN != 0 || len(store.deleteByScopeArgs) != 0 {
		t.Errorf("empty scope must short-circuit; got deleted=%d clear=%d delete=%d",
			state.Deleted, r.clearN, len(store.deleteByScopeArgs))
	}
}

func TestForgetAll_RequiredProjectionFailureAborts(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "alice"}
	store := &fakeStore{
		facts:            []domain.TemporalFact{{ID: "f1", Scope: scope}},
		deleteScopeCount: 1,
	}
	bad := &fakeProjection{name: "retrieval", level: port.Required, clearErr: errors.New("boom")}
	runner := newRunner(t, store, []port.Projection{bad}, nil)

	state := &forget.State{
		Scope:           scope,
		Mode:            domain.ForgetHard,
		ConfirmScopeKey: scope.PartitionKey(),
	}
	err := runner.Run(context.Background(), state)
	if err == nil {
		t.Fatalf("expected error from required projection")
	}
	if len(store.deleteByScopeArgs) != 0 {
		t.Errorf("store.DeleteByScope must NOT run after required failure")
	}
}

// TestForgetAll_ExpireFilter_DeletesOnlyExpiredFacts pins the
// ExpireRetired path (Cluster A / D5 2026-05-21). A non-nil Filter
// switches the stage to per-id Hard delete, leaving non-matching
// facts intact and emitting ExpireRetiredDetail with Scanned /
// Deleted counters.
func TestForgetAll_ExpireFilter_DeletesOnlyExpiredFacts(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "alice"}
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	store := &fakeStore{
		facts: []domain.TemporalFact{
			{ID: "expired-1", Scope: scope, ExpiresAt: &past},
			{ID: "expired-2", Scope: scope, ExpiresAt: &past},
			{ID: "future", Scope: scope, ExpiresAt: &future},
			{ID: "open", Scope: scope},
		},
	}
	r := &fakeProjection{name: "retrieval", level: port.Required}
	runner := newRunner(t, store, []port.Projection{r}, nil)

	state := &forget.State{
		Scope:  scope,
		Filter: &forget.ForgetFilter{ExpiresBefore: &now},
		Now:    now,
	}
	state.EnsureTrace()
	if err := runner.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Deleted != 2 {
		t.Errorf("Deleted = %d, want 2", state.Deleted)
	}
	if len(store.deleteIDs) != 1 || len(store.deleteIDs[0]) != 2 {
		t.Errorf("delete ids = %+v, want one batch of 2", store.deleteIDs)
	}
	if len(store.deleteByScopeArgs) != 0 {
		t.Errorf("expire filter must NOT invoke DeleteByScope (scope wipe)")
	}
	if r.clearN != 0 {
		t.Errorf("expire filter must NOT invoke ClearScope; got %d", r.clearN)
	}
	d, ok := state.Trace.Stages[0].Detail.(diagnostic.ExpireRetiredDetail)
	if !ok {
		t.Fatalf("detail = %T, want ExpireRetiredDetail", state.Trace.Stages[0].Detail)
	}
	if d.Scanned != 4 || d.Deleted != 2 {
		t.Errorf("detail mismatch: %+v", d)
	}
}

// TestForgetAll_ExpireFilter_NoMatchesReturnsZero pins the empty
// case: a sweep that finds nothing returns Deleted=0 with a non-
// error detail (no DeleteByScope, no per-id Delete).
func TestForgetAll_ExpireFilter_NoMatchesReturnsZero(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "alice"}
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	store := &fakeStore{
		facts: []domain.TemporalFact{
			{ID: "future", Scope: scope, ExpiresAt: &future},
			{ID: "open", Scope: scope},
		},
	}
	runner := newRunner(t, store, nil, nil)
	state := &forget.State{
		Scope:  scope,
		Filter: &forget.ForgetFilter{ExpiresBefore: &now},
		Now:    now,
	}
	state.EnsureTrace()
	if err := runner.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0", state.Deleted)
	}
	if len(store.deleteIDs) != 0 {
		t.Errorf("no expired → no Delete call; got %v", store.deleteIDs)
	}
}

// TestForgetAll_ExpireFilter_SkipsConfirmScopeKey pins the D5
// 2026-05-21 carve-out: ExpireRetired waives the GDPR confirm-key
// guard because a TTL sweep is a non-irreversible administrative
// action, not an Art.17 wipe. The same call without Filter would
// require ConfirmScopeKey under Hard mode.
func TestForgetAll_ExpireFilter_SkipsConfirmScopeKey(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "alice"}
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	store := &fakeStore{
		facts: []domain.TemporalFact{{ID: "expired", Scope: scope, ExpiresAt: &past}},
	}
	runner := newRunner(t, store, nil, nil)
	state := &forget.State{
		Scope:           scope,
		Mode:            domain.ForgetHard,
		Filter:          &forget.ForgetFilter{ExpiresBefore: &now},
		ConfirmScopeKey: "",
		Now:             now,
	}
	if err := runner.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", state.Deleted)
	}
}

func TestForgetAll_OptionalProjectionFailureTolerated(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "alice"}
	store := &fakeStore{
		facts:            []domain.TemporalFact{{ID: "f1", Scope: scope}},
		deleteScopeCount: 1,
	}
	r := &fakeProjection{name: "retrieval", level: port.Required}
	opt := &fakeProjection{name: "graph", level: port.Optional, clearErr: errors.New("flaky")}
	runner := newRunner(t, store, []port.Projection{r, opt}, nil)

	state := &forget.State{
		Scope:           scope,
		Mode:            domain.ForgetHard,
		ConfirmScopeKey: scope.PartitionKey(),
	}
	state.EnsureTrace()
	if err := runner.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", state.Deleted)
	}
	d := state.Trace.Stages[0].Detail.(diagnostic.ForgetAllDetail)
	if d.ProjectionsCleared != 1 {
		t.Errorf("ProjectionsCleared = %d, want 1 (only required cleared cleanly)", d.ProjectionsCleared)
	}
}
