package recall

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/asyncsemantic"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

func TestForgetAll_HardWrongConfirmDoesNotBumpScopeGen(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	startGen := mem.(*memory).peekScopeGen(scope)
	_, err = mem.ForgetAll(ctx, scope, ForgetHard, scope.CanonicalKey())
	if !errors.Is(err, ErrScopeKeyMismatch) {
		t.Fatalf("ForgetAll err = %v, want ErrScopeKeyMismatch", err)
	}
	if got := mem.(*memory).peekScopeGen(scope); got != startGen {
		t.Fatalf("wrong confirm must not bump scope gen: before=%d after=%d", startGen, got)
	}
	res, err := mem.Save(ctx, scope, SaveRequest{Facts: []TemporalFact{{
		ID: "ok", Kind: domain.KindState, Subject: "a", Predicate: "b", Content: "c",
	}}})
	if err != nil {
		t.Fatalf("Save after failed ForgetAll confirm: %v", err)
	}
	if len(res.FactIDs) == 0 {
		t.Fatal("expected save to succeed when ForgetAll confirm mismatched")
	}
}

func TestForgetAll_HardRequiresPartitionKeyNotAgentCanonical(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	agentScope := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	_, err = mem.Save(ctx, agentScope, SaveRequest{Facts: []TemporalFact{{
		ID: "f1", Kind: domain.KindState, Subject: "alice", Predicate: "city", Content: "Paris",
	}}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err = mem.ForgetAll(ctx, agentScope, ForgetHard, agentScope.CanonicalKey())
	if !errors.Is(err, ErrScopeKeyMismatch) {
		t.Fatalf("ForgetAll with agent canonical key must mismatch, got %v", err)
	}
	if _, err = mem.ForgetAll(ctx, agentScope, ForgetHard, agentScope.PartitionKey()); err != nil {
		t.Fatalf("ForgetAll with partition key: %v", err)
	}
	if _, err = store.Get(ctx, agentScope, "f1"); !errors.Is(err, temporalstore.ErrNotFound) {
		t.Fatalf("hard forget must wipe user partition")
	}
}

func TestForgetAll_HardPurgeScopeRemovesCompletedOutboxPII(t *testing.T) {
	queue := asyncsemantic.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store), WithAsyncSemanticQueue(queue))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	job := port.AsyncSemanticJob{
		RequestID:      "async-1",
		Scope:          scope,
		EpisodeFactIDs: []string{"ep-1"},
		TurnsSnapshot:  []domain.TurnContext{{Text: "secret user message"}},
	}
	if _, err := queue.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	jobs, err := queue.Claim(ctx, port.AsyncSemanticClaimOptions{Max: 1, Now: time.Now()})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: %v jobs=%v", err, jobs)
	}
	if err := queue.Complete(ctx, jobs[0].RequestID, jobs[0].LeaseToken, port.AsyncSemanticResult{}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.ForgetAll(ctx, scope, ForgetHard, scope.PartitionKey()); err != nil {
		t.Fatalf("ForgetAll: %v", err)
	}
	stats, err := queue.Stats(ctx, port.AsyncSemanticStatsFilter{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Completed != 0 || stats.Pending != 0 || stats.Leased != 0 {
		t.Fatalf("PurgeScope must clear partition queue, stats=%+v", stats)
	}
}

func TestSave_AbortsWhenForgetAllHardDuringIngest(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	ingestStarted := make(chan struct{})
	releaseIngest := make(chan struct{})
	mem, err := New(withTemporalStore(store), withCompiler(&barrierIngestor{
		onStart: func() { close(ingestStarted) },
		wait:    func() { <-releaseIngest },
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	var wg sync.WaitGroup
	wg.Add(1)
	var saveErr error
	go func() {
		defer wg.Done()
		_, saveErr = mem.Save(ctx, scope, SaveRequest{Facts: []TemporalFact{{
			ID: "late", Kind: domain.KindState, Subject: "a", Predicate: "b", Content: "c",
			MergeKey: "state|a|b",
		}}})
	}()
	<-ingestStarted
	if _, err := mem.ForgetAll(ctx, scope, ForgetHard, scope.PartitionKey()); err != nil {
		t.Fatalf("ForgetAll: %v", err)
	}
	close(releaseIngest)
	wg.Wait()
	if saveErr == nil {
		t.Fatal("Save must abort when partition wiped during ingest")
	}
	if !errdefs.IsAborted(saveErr) {
		t.Fatalf("save err = %v, want aborted", saveErr)
	}
	if _, err := store.Get(ctx, scope, "late"); !errors.Is(err, temporalstore.ErrNotFound) {
		t.Fatalf("aborted save must not leave facts, got %v", err)
	}
}

func TestSaveExplain_DefaultRedactsDroppedFacts(t *testing.T) {
	mem, err := New(withCompiler(&rejectAllIngestor{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	_, trace, err := mem.(SaveExplainer).SaveExplain(ctx, scope, SaveRequest{Facts: []TemporalFact{{
		ID: "f1", Kind: domain.KindState, Content: "secret body",
	}}})
	if err != nil {
		t.Fatalf("SaveExplain: %v", err)
	}
	drops := diagnostic.ExtractSaveDropped(trace.Stages)
	if len(drops) == 0 {
		t.Fatal("expected dropped facts in trace")
	}
	if drops[0].Fact != nil {
		t.Fatalf("default SaveExplain must redact Fact payload, got %T", drops[0].Fact)
	}
	if drops[0].ContentHash == "" {
		t.Fatalf("expected content hash in redacted drop, got %+v", drops[0])
	}
}

func TestSaveExplainDebug_RetainsDroppedFactPayload(t *testing.T) {
	mem, err := New(withCompiler(&rejectAllIngestor{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	_, trace, err := mem.(SaveDebugExplainer).SaveExplainDebug(ctx, scope, SaveRequest{Facts: []TemporalFact{{
		ID: "f1", Kind: domain.KindState, Content: "secret body",
	}}})
	if err != nil {
		t.Fatalf("SaveExplainDebug: %v", err)
	}
	drops := diagnostic.ExtractSaveDropped(trace.Stages)
	if len(drops) == 0 || drops[0].Fact == nil {
		t.Fatalf("debug explain must retain raw drops, got %+v", drops)
	}
}

type rejectAllIngestor struct{}

func (rejectAllIngestor) Compile(_ context.Context, in port.IngestInput) (port.IngestResult, error) {
	facts := make([]domain.TemporalFact, len(in.Facts))
	for i, f := range in.Facts {
		facts[i] = f
		if facts[i].Scope.RuntimeID == "" {
			facts[i].Scope = in.Scope
		}
	}
	return port.IngestResult{
		Facts: facts,
		Dropped: []diagnostic.DroppedFact{{
			Fact: facts[0], Reason: "policy:reject",
		}},
	}, nil
}

func TestResolver_ExplicitSupersedesRejectsCrossAgentPrior(t *testing.T) {
	prior := domain.TemporalFact{
		ID: "priv-b", Scope: domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"},
		Kind: domain.KindState, MergeKey: "k", Content: "old",
	}
	view := &ingestTestView{facts: []domain.TemporalFact{prior}}
	r := ingest.NewResolver()
	_, err := r.ResolveConflicts(context.Background(), view, []domain.TemporalFact{{
		ID: "new-a", Scope: domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"},
		Kind: domain.KindState, Supersedes: []string{"priv-b"}, MergeKey: "k", Content: "new",
	}})
	if err == nil {
		t.Fatal("expected validation error for cross-agent explicit supersede")
	}
}

type ingestTestView struct {
	facts []domain.TemporalFact
}

func (v *ingestTestView) FindByMergeKey(context.Context, domain.Scope, string) ([]domain.TemporalFact, error) {
	return nil, nil
}

func (v *ingestTestView) Get(_ context.Context, _ domain.Scope, factID string) (domain.TemporalFact, error) {
	for _, f := range v.facts {
		if f.ID == factID {
			return f, nil
		}
	}
	return domain.TemporalFact{}, errdefs.NotFoundf("missing")
}

type barrierIngestor struct {
	onStart func()
	wait    func()
}

func (b *barrierIngestor) Compile(ctx context.Context, in port.IngestInput) (port.IngestResult, error) {
	if b.onStart != nil {
		b.onStart()
	}
	if b.wait != nil {
		select {
		case <-ctx.Done():
			return port.IngestResult{}, ctx.Err()
		default:
			b.wait()
		}
	}
	facts := make([]domain.TemporalFact, len(in.Facts))
	for i, f := range in.Facts {
		facts[i] = f
		if facts[i].Scope.RuntimeID == "" {
			facts[i].Scope = in.Scope
		}
	}
	return port.IngestResult{Facts: facts}, nil
}
