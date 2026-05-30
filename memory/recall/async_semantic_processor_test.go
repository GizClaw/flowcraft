package recall

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	writestages "github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/store/asyncsemantic"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
)

// turnNoteExtractor feeds the default ingest pipeline one note per
// turn without LLM calls.
type turnNoteExtractor struct{}

func (turnNoteExtractor) Extract(_ context.Context, input port.IngestInput) ([]domain.TemporalFact, error) {
	var facts []domain.TemporalFact
	for _, turn := range input.Turns {
		if turn.Text == "" {
			continue
		}
		facts = append(facts, domain.TemporalFact{
			Kind:    domain.KindNote,
			Content: turn.Text,
		})
	}
	return facts, nil
}

func testSemanticIngestor() port.Ingestor {
	return ingest.New(ingest.Stages{Extractor: turnNoteExtractor{}})
}

type deletingEpisodeExtractor struct {
	store port.TemporalStore
	scope Scope
}

func (e deletingEpisodeExtractor) Extract(ctx context.Context, input port.IngestInput) ([]domain.TemporalFact, error) {
	facts, err := e.store.List(ctx, e.scope, port.ListQuery{Kinds: []domain.FactKind{domain.KindEpisode}, IncludeSuperseded: true})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, f.ID)
	}
	if len(ids) > 0 {
		if err := e.store.Delete(ctx, e.scope, ids); err != nil {
			return nil, err
		}
	}
	return turnNoteExtractor{}.Extract(ctx, input)
}

type cancelScopeFailQueue struct {
	*asyncsemantic.Queue
	err error
}

func (q *cancelScopeFailQueue) CancelScope(context.Context, domain.Scope) (int, error) {
	return 0, q.err
}

func (q *cancelScopeFailQueue) CancelMatchingEpisodes(ctx context.Context, scope domain.Scope, ids []string) (int, error) {
	return q.Queue.CancelMatchingEpisodes(ctx, scope, ids)
}

func TestProcessAsyncSemantic_DerivesRecallableFacts(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		WithTemporalStore(store),
		withCompiler(testSemanticIngestor()),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("processor not available")
	}
	scope := asyncTestScope()
	ctx := context.Background()

	res, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "paris trip"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.AsyncRequestID == "" {
		t.Fatal("AsyncRequestID empty")
	}

	out, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{
		WorkerID: "test-worker",
		Limit:    4,
		Scope:    scope,
	})
	if err != nil {
		t.Fatalf("ProcessAsyncSemantic: %v", err)
	}
	if out.Claimed != 1 || out.Completed != 1 {
		t.Fatalf("process result = %+v, want claimed=1 completed=1", out)
	}
	drainSideEffectsForTest(t, mem, scope)

	hits, err := mem.Recall(ctx, scope, Query{Text: "paris", Limit: 5})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("Recall must surface semantic facts after processor drain")
	}
	for _, h := range hits {
		if h.Fact.Origin.Kind != OriginKindSemanticDerivation {
			t.Errorf("hit origin kind = %q, want semantic_derivation", h.Fact.Origin.Kind)
		}
		if h.Fact.Origin.RequestID != res.AsyncRequestID {
			t.Errorf("hit origin request = %q, want %q", h.Fact.Origin.RequestID, res.AsyncRequestID)
		}
	}
}

func TestProcessAsyncSemantic_IdempotentRecovery(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		WithTemporalStore(store),
		withCompiler(testSemanticIngestor()),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("processor not available")
	}
	scope := asyncTestScope()
	ctx := context.Background()

	res, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "tokyo"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate crash after append but before Complete.
	now := time.Now()
	validFrom := now
	sem := domain.TemporalFact{
		ID:         "sem-manual-1",
		Scope:      scope,
		Kind:       domain.KindNote,
		Content:    "tokyo",
		Entities:   []string{"tokyo"},
		ObservedAt: now,
		ValidFrom:  &validFrom,
		Origin: domain.FactOrigin{
			RequestID: res.AsyncRequestID,
			Kind:      domain.OriginKindSemanticDerivation,
		},
	}
	if err := store.Append(ctx, []domain.TemporalFact{sem}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	out, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{Limit: 1, Scope: scope})
	if err != nil {
		t.Fatalf("ProcessAsyncSemantic: %v", err)
	}
	if out.Recovered != 1 {
		t.Fatalf("result = %+v, want recovered=1 (idempotent complete)", out)
	}
	if out.Completed != 1 {
		t.Fatalf("result = %+v, want completed=1", out)
	}

	facts, err := store.FindByOriginRequestID(ctx, scope, res.AsyncRequestID)
	if err != nil {
		t.Fatalf("FindByOriginRequestID: %v", err)
	}
	var semantic int
	for _, f := range facts {
		if f.Origin.Kind == domain.OriginKindSemanticDerivation {
			semantic++
		}
	}
	if semantic != 1 {
		t.Fatalf("semantic_derivation facts = %d, want 1 (no duplicate append)", semantic)
	}
}

func TestQueue_ClaimScopeFilter(t *testing.T) {
	q := asyncsemantic.New()
	ctx := context.Background()
	scopeA := Scope{RuntimeID: "rt-a", UserID: "u1"}
	scopeB := Scope{RuntimeID: "rt-b", UserID: "u1"}
	_, _ = q.Enqueue(ctx, port.AsyncSemanticJob{RequestID: "a1", Scope: scopeA})
	_, _ = q.Enqueue(ctx, port.AsyncSemanticJob{RequestID: "b1", Scope: scopeB})

	jobs, err := q.Claim(ctx, port.AsyncSemanticClaimOptions{
		WorkerID: "w",
		Now:      time.Now(),
		Max:      10,
		Scope:    &scopeA,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 || jobs[0].RequestID != "a1" {
		t.Fatalf("scoped claim = %+v, want only a1", jobs)
	}
}

func TestProcessAsyncSemantic_DeletedEpisodePermanentFail(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		WithTemporalStore(store),
		withCompiler(testSemanticIngestor()),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("processor not available")
	}
	scope := asyncTestScope()
	ctx := context.Background()

	res, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "gone"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(ctx, scope, res.EpisodeFactIDs); err != nil {
		t.Fatalf("Delete episode: %v", err)
	}

	out, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{Limit: 1, Scope: scope})
	if err != nil {
		t.Fatalf("ProcessAsyncSemantic: %v", err)
	}
	if out.Failed != 1 || out.Completed != 0 {
		t.Fatalf("result = %+v, want failed=1 completed=0", out)
	}
	semantic, _ := store.FindByOriginRequestID(ctx, scope, res.AsyncRequestID)
	for _, f := range semantic {
		if f.Origin.Kind == domain.OriginKindSemanticDerivation {
			t.Fatalf("must not derive semantic facts after episode delete, got %+v", f)
		}
	}
}

func TestProcessAsyncSemantic_EpisodeDeletedDuringWorkerDoesNotDerive(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	scope := asyncTestScope()
	mem, err := New(
		WithTemporalStore(store),
		withCompiler(ingest.New(ingest.Stages{Extractor: deletingEpisodeExtractor{store: store, scope: scope}})),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("processor not available")
	}
	ctx := context.Background()

	res, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "deleted during worker"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{Limit: 1, Scope: scope})
	if err != nil {
		t.Fatalf("ProcessAsyncSemantic: %v", err)
	}
	if out.Failed != 1 || out.Completed != 0 {
		t.Fatalf("result = %+v, want failed=1 completed=0 when episode disappears during worker", out)
	}
	semantic, _ := store.FindByOriginRequestID(ctx, scope, res.AsyncRequestID)
	for _, f := range semantic {
		if f.Origin.Kind == domain.OriginKindSemanticDerivation {
			t.Fatalf("must not derive semantic facts after concurrent episode delete, got %+v", f)
		}
	}
}

func TestProcessAsyncSemantic_RequiresPartitionScope(t *testing.T) {
	queue := asyncsemantic.New()
	mem, err := New(
		withCompiler(testSemanticIngestor()),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	_, err = proc.ProcessAsyncSemantic(context.Background(), AsyncSemanticProcessOptions{Limit: 1})
	if err == nil {
		t.Fatal("ProcessAsyncSemantic without Scope must fail")
	}
	_, err = proc.ProcessAsyncSemantic(context.Background(), AsyncSemanticProcessOptions{Limit: 1, RuntimeID: "rt"})
	if err == nil {
		t.Fatal("ProcessAsyncSemantic with RuntimeID-only drain must fail")
	}
}

func TestProcessAsyncSemantic_LeaseRecycleRecoversWithoutReappend(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		WithTemporalStore(store),
		withCompiler(testSemanticIngestor()),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	m := mem.(*memory)
	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	scope := asyncTestScope()
	ctx := context.Background()

	res, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Text: "lease recycle"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	jobs, err := queue.Claim(ctx, port.AsyncSemanticClaimOptions{
		WorkerID: "w-a",
		Now:      time.Now(),
		Max:      1,
		Scope:    &scope,
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: %v jobs=%d", err, len(jobs))
	}
	job := jobs[0]

	// Worker A appends semantic facts but never calls Complete (simulates
	// crash before ack). Lease expiry makes the job claimable again.
	turns, err := writestages.ReconstructTurnsForJob(ctx, store, job)
	if err != nil {
		t.Fatalf("ReconstructTurns: %v", err)
	}
	state := &write.WriteState{
		Scope:          scope,
		Turns:          turns,
		AsyncRequestID: job.RequestID,
		SemanticDerivationOrigin: domain.FactOrigin{
			RequestID:      job.RequestID,
			Kind:           domain.OriginKindSemanticDerivation,
			EpisodeFactIDs: append([]string(nil), job.EpisodeFactIDs...),
		},
	}
	state.EnsureTrace()
	if err := m.asyncSemanticWorkerPreRunner.Run(ctx, state); err != nil {
		t.Fatalf("pre-runner: %v", err)
	}
	unlock := m.lockWriteScope(scope)
	if err := m.asyncSemanticWorkerPostRunner.Run(ctx, state); err != nil {
		unlock()
		t.Fatalf("post-runner: %v", err)
	}
	unlock()

	expiredNow := time.Now().Add(25 * time.Hour)
	out, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{
		Limit: 1,
		Scope: scope,
		Now:   expiredNow,
	})
	if err != nil {
		t.Fatalf("ProcessAsyncSemantic: %v", err)
	}
	if out.Recovered != 1 {
		t.Fatalf("result = %+v, want recovered=1 after lease recycle", out)
	}
	facts, err := store.FindByOriginRequestID(ctx, scope, res.AsyncRequestID)
	if err != nil {
		t.Fatalf("FindByOriginRequestID: %v", err)
	}
	var semantic int
	for _, f := range facts {
		if f.Origin.Kind == domain.OriginKindSemanticDerivation {
			semantic++
		}
	}
	if semantic != 1 {
		t.Fatalf("semantic_derivation facts = %d, want 1 (no duplicate append)", semantic)
	}
}

func TestProcessAsyncSemantic_ConcurrentDrainSingleCompletion(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		WithTemporalStore(store),
		withCompiler(testSemanticIngestor()),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("processor not available")
	}
	scope := asyncTestScope()
	ctx := context.Background()
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Text: "once"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	var completed atomic.Int32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			out, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{
				WorkerID: "w",
				Limit:    1,
				Scope:    scope,
			})
			if err != nil {
				return
			}
			completed.Add(int32(out.Completed))
		}()
	}
	wg.Wait()
	if got := int(completed.Load()); got != 1 {
		t.Fatalf("total completed across workers = %d, want 1", got)
	}
}

func TestExpireRetired_DoesNotCancelUnrelatedAsyncJobs(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(WithTemporalStore(store), WithAsyncSemanticQueue(queue))
	if err != nil {
		t.Fatal(err)
	}
	scope := asyncTestScope()
	ctx := context.Background()
	expiredAt := time.Now().Add(-time.Hour)

	resA, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t-exp", Text: "ttl episode"}},
	})
	if err != nil {
		t.Fatalf("save expired episode job: %v", err)
	}
	if len(resA.EpisodeFactIDs) != 1 {
		t.Fatalf("episode fact IDs = %v, want 1", resA.EpisodeFactIDs)
	}
	resB, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t-ok", Text: "keep"}},
	})
	if err != nil {
		t.Fatalf("save keep episode job: %v", err)
	}

	epID := resA.EpisodeFactIDs[0]
	f, err := store.Get(ctx, scope, epID)
	if err != nil {
		t.Fatalf("Get episode: %v", err)
	}
	f.ExpiresAt = &expiredAt
	if err := store.Delete(ctx, scope, []string{epID}); err != nil {
		t.Fatalf("Delete episode: %v", err)
	}
	if err := store.Append(ctx, []domain.TemporalFact{f}); err != nil {
		t.Fatalf("re-append expired episode: %v", err)
	}

	if _, err := mem.ExpireRetired(ctx, scope, time.Now()); err != nil {
		t.Fatalf("ExpireRetired: %v", err)
	}
	if d := queueDepth(t, queue); d != 1 {
		t.Fatalf("unrelated async job must remain, queue depth = %d, want 1", d)
	}
	// queueDepth leases the survivor; advance past lease TTL to reclaim.
	jobs, err := claimBatch(ctx, queue, "worker", time.Now().Add(25*time.Hour), 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 || jobs[0].RequestID != resB.AsyncRequestID {
		t.Fatalf("remaining job = %+v, want only %q", jobs, resB.AsyncRequestID)
	}
}

type completeFailQueue struct {
	*asyncsemantic.Queue
	err error
}

func (q *completeFailQueue) Complete(context.Context, string, string, port.AsyncSemanticResult) error {
	return q.err
}

func TestProcessAsyncSemantic_CompleteAckFailureCountsFailed(t *testing.T) {
	ackErr := errors.New("queue complete unavailable")
	queue := &completeFailQueue{Queue: asyncsemantic.New(), err: ackErr}
	mem, err := New(
		WithTemporalStore(temporalstore.NewMemoryStore()),
		withCompiler(testSemanticIngestor()),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	proc, ok := NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("processor missing")
	}
	scope := asyncTestScope()
	ctx := context.Background()
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Text: "ack fail"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{Limit: 1, Scope: scope})
	if err != nil {
		t.Fatalf("ProcessAsyncSemantic: %v", err)
	}
	if out.Failed != 1 {
		t.Fatalf("result = %+v, want failed=1 when Complete ack fails", out)
	}
}

func TestForgetAllHard_EmitsAsyncJobsCancelledTelemetry(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		WithTemporalStore(store),
		WithAsyncSemanticQueue(queue),
		WithTelemetryHook(hook),
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := asyncTestScope()
	ctx := context.Background()
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Text: "cancel me"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	hook.stages = nil
	if _, err := mem.ForgetAll(ctx, scope, ForgetHard, scope.PartitionKey()); err != nil {
		t.Fatalf("ForgetAll: %v", err)
	}
	var forgetStage *diagnostic.StageDiagnostic
	for i := range hook.stages {
		if hook.stages[i].Stage != "forget_all" {
			continue
		}
		d, ok := hook.stages[i].Detail.(diagnostic.ForgetAllDetail)
		if !ok || d.AsyncJobsCancelled == 0 {
			continue
		}
		forgetStage = &hook.stages[i]
	}
	if forgetStage == nil {
		t.Fatalf("telemetry stages = %v, want forget_all with async cancel", stageNames(hook.stages))
	}
	if forgetStage.Status != diagnostic.StatusOK {
		t.Fatalf("forget_all status = %s, want ok", forgetStage.Status)
	}
	d, ok := forgetStage.Detail.(diagnostic.ForgetAllDetail)
	if !ok {
		t.Fatalf("detail = %T, want ForgetAllDetail", forgetStage.Detail)
	}
	if d.AsyncJobsCancelled != 2 {
		t.Fatalf("AsyncJobsCancelled = %d, want 2 (semantic + side-effect outbox)", d.AsyncJobsCancelled)
	}
}

func TestForgetAllHard_CancelsPendingAsyncJobs(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(WithTemporalStore(store), WithAsyncSemanticQueue(queue))
	if err != nil {
		t.Fatal(err)
	}
	scope := asyncTestScope()
	ctx := context.Background()
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Text: "x"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if d := queueDepth(t, queue); d != 1 {
		t.Fatalf("queue depth = %d, want 1 before ForgetAll", d)
	}
	if _, err := mem.ForgetAll(ctx, scope, ForgetHard, scope.PartitionKey()); err != nil {
		t.Fatalf("ForgetAll: %v", err)
	}
	if d := queueDepth(t, queue); d != 0 {
		t.Fatalf("queue depth = %d, want 0 after ForgetAll", d)
	}
}

func TestForgetAllHard_CancelScopeFailureSurfaces(t *testing.T) {
	queue := &cancelScopeFailQueue{Queue: asyncsemantic.New(), err: errors.New("queue unavailable")}
	mem, err := New(WithTemporalStore(temporalstore.NewMemoryStore()), WithAsyncSemanticQueue(queue))
	if err != nil {
		t.Fatal(err)
	}
	scope := asyncTestScope()
	ctx := context.Background()
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Text: "x"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err = mem.ForgetAll(ctx, scope, ForgetHard, scope.PartitionKey())
	if err == nil {
		t.Fatal("ForgetAll(Hard) must surface async job cleanup failures")
	}
	if !errors.Is(err, queue.err) {
		t.Fatalf("ForgetAll error = %v, want to wrap %v", err, queue.err)
	}
}

func TestExpireRetiredNoMatch_DoesNotCancelAsyncJobs(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(WithTemporalStore(store), WithAsyncSemanticQueue(queue))
	if err != nil {
		t.Fatal(err)
	}
	scope := asyncTestScope()
	ctx := context.Background()
	if _, err := mem.Save(ctx, scope, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Text: "still pending"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	deleted, err := mem.ExpireRetired(ctx, scope, time.Now())
	if err != nil {
		t.Fatalf("ExpireRetired: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	jobs, err := claimBatch(ctx, queue, "worker", time.Now(), 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("ExpireRetired with no matches must leave pending async job, got %+v", jobs)
	}
}
