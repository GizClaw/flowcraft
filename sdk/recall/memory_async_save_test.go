package recall

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/asyncsemantic"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

// asyncTestScope is the shared canonical scope used by the F.1a
// memory facade tests. Tests rely on a stable scope so the
// in-memory queue's FIFO is observable across cases.
func asyncTestScope() Scope { return Scope{RuntimeID: "rt", UserID: "u1"} }

// stubLLM is a noop LLM client that records every Generate call. The
// async semantic write contract forbids LLM invocation on the user-
// facing Save path; tests use this to assert "calls == 0".
type stubLLM struct {
	calls atomic.Int64
}

func (l *stubLLM) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	l.calls.Add(1)
	return llm.Message{}, llm.TokenUsage{}, nil
}

func (l *stubLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	l.calls.Add(1)
	return nil, errors.New("stubLLM: streaming not implemented")
}

// failingQueue rejects every Enqueue with the supplied sentinel so
// tests can drive the outbox-failure compensation path.
type failingQueue struct {
	err error
}

func (q *failingQueue) Enqueue(context.Context, port.AsyncSemanticJob) (port.AsyncSemanticReceipt, error) {
	return port.AsyncSemanticReceipt{}, q.err
}
func (q *failingQueue) Cancel(context.Context, string) error { return nil }
func (q *failingQueue) Claim(context.Context, string, time.Time, int) ([]port.AsyncSemanticJob, error) {
	return nil, nil
}
func (q *failingQueue) Complete(context.Context, string, port.AsyncSemanticResult) error { return nil }
func (q *failingQueue) Fail(context.Context, string, port.AsyncSemanticFailure) error    { return nil }

// queueDepth peeks into the in-memory queue by claiming and then
// silently dropping the claimed jobs. Tests that need to assert
// "no enqueue happened" use this before discarding the queue.
func queueDepth(t *testing.T, q *asyncsemantic.Queue) int {
	t.Helper()
	jobs, err := q.Claim(context.Background(), "test", time.Now(), 1024)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return len(jobs)
}

// TestSave_DefaultSyncMode_Unchanged pins the F.1a invariant: a
// zero-Mode SaveRequest with structured Facts behaves exactly like
// pre-F.1 Save — one canonical fact, no async metadata, no episode
// row, no queue activity even when the queue option is wired.
func TestSave_DefaultSyncMode_Unchanged(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "sync"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Errorf("FactIDs len = %d, want 1", len(res.FactIDs))
	}
	if res.AsyncRequestID != "" || res.SemanticPending || len(res.EpisodeFactIDs) != 0 {
		t.Errorf("async fields must stay zero in sync mode, got %+v", res)
	}
	if d := queueDepth(t, queue); d != 0 {
		t.Errorf("sync save must not enqueue: depth = %d", d)
	}
}

// TestSave_AsyncWithoutQueue_ReturnsValidationError pins §4.3 — Save
// MUST refuse WriteModeAsyncSemantic with non-empty Turns when no
// queue is configured, instead of silently falling back to sync
// (which would defeat the latency contract callers chose the mode for).
func TestSave_AsyncWithoutQueue_ReturnsValidationError(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store))
	if err != nil {
		t.Fatal(err)
	}
	_, err = mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "hi"}},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("err = %v, want validation classification", err)
	}
	facts, _ := store.List(context.Background(), asyncTestScope(), port.ListQuery{IncludeSuperseded: true})
	if len(facts) != 0 {
		t.Errorf("validation error must not write to store: %d facts present", len(facts))
	}
}

// TestSave_AsyncEmptyTurns_DegradesToSync pins §3.1 — when Turns is
// empty the async mode degrades to the sync path. Structured facts
// land synchronously, no episode fact is built, no job is queued.
func TestSave_AsyncEmptyTurns_DegradesToSync(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Facts: []TemporalFact{{Kind: FactNote, Content: "structured"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.AsyncRequestID != "" || res.SemanticPending {
		t.Errorf("degraded sync path must not emit async correlation: %+v", res)
	}
	if len(res.FactIDs) != 1 {
		t.Errorf("FactIDs len = %d, want 1", len(res.FactIDs))
	}
	if d := queueDepth(t, queue); d != 0 {
		t.Errorf("degrade-to-sync must not enqueue: depth = %d", d)
	}
	facts, _ := store.List(context.Background(), asyncTestScope(), port.ListQuery{IncludeSuperseded: true})
	for _, f := range facts {
		if f.Kind == FactEpisode {
			t.Errorf("degrade-to-sync must not write episode fact: %+v", f)
		}
	}
}

// TestSave_AsyncTurnsOnly_WritesEpisodeAndEnqueues pins the §5.2
// happy-path contract — a Turns-only async save produces one
// episode fact, one queued job, and NEVER calls the LLM extractor.
func TestSave_AsyncTurnsOnly_WritesEpisodeAndEnqueues(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	llmClient := &stubLLM{}
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
		WithLLMExtractor(llmClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Mode: WriteModeAsyncSemantic,
		Turns: []TurnContext{
			{ID: "t1", EvidenceID: "ev1", Role: "user", Speaker: "Alice", Text: "I love Paris"},
		},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.AsyncRequestID == "" {
		t.Error("AsyncRequestID empty")
	}
	if !res.SemanticPending {
		t.Error("SemanticPending = false")
	}
	if len(res.EpisodeFactIDs) != 1 {
		t.Errorf("EpisodeFactIDs len = %d, want 1", len(res.EpisodeFactIDs))
	}
	if len(res.FactIDs) != 1 || res.FactIDs[0] != res.EpisodeFactIDs[0] {
		t.Errorf("FactIDs = %v vs EpisodeFactIDs = %v", res.FactIDs, res.EpisodeFactIDs)
	}
	if llmClient.calls.Load() != 0 {
		t.Errorf("LLM called %d times, must not invoke extractor on async sync lane", llmClient.calls.Load())
	}
	facts, _ := store.List(context.Background(), asyncTestScope(), port.ListQuery{IncludeSuperseded: true})
	var episodes int
	for _, f := range facts {
		if f.Kind == FactEpisode {
			episodes++
			if f.Origin.RequestID != res.AsyncRequestID {
				t.Errorf("episode Origin.RequestID = %q, want %q", f.Origin.RequestID, res.AsyncRequestID)
			}
		}
	}
	if episodes != 1 {
		t.Errorf("store has %d episode facts, want 1", episodes)
	}
	if d := queueDepth(t, queue); d != 1 {
		t.Errorf("queue depth = %d, want 1", d)
	}
}

type countEvolution struct {
	afterSave int
}

func (c *countEvolution) AfterSave(_ context.Context, _ domain.Scope, _ []string) error {
	c.afterSave++
	return nil
}
func (*countEvolution) AfterRecall(_ context.Context, _ domain.Scope, _ domain.RecallTrace) error {
	return nil
}

// TestSave_AsyncTurnsOnly_SkipsEvolutionAfterSave pins §5.3: the sync
// async lane must not invoke AfterSave on episode-only saves; semantic
// evolution runs in the background worker after derivation.
func TestSave_AsyncTurnsOnly_SkipsEvolutionAfterSave(t *testing.T) {
	ev := &countEvolution{}
	mem, err := New(
		withTemporalStore(temporalstore.NewMemoryStore()),
		WithAsyncSemanticQueue(asyncsemantic.New()),
		WithEvolution(ev),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	if _, err := mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "hi"}},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if ev.afterSave != 0 {
		t.Errorf("AfterSave calls = %d, want 0 for turns-only async save", ev.afterSave)
	}
}

// TestSave_AsyncMixedFactsAndTurns_BothPaths pins §5.2 — structured
// facts go through the sync semantic pipeline, turns go through the
// episode lane, and SaveResult merges both.
func TestSave_AsyncMixedFactsAndTurns_BothPaths(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Facts: []TemporalFact{{Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris"}},
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "anything"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(res.FactIDs) != 2 {
		t.Fatalf("FactIDs len = %d, want 2 (sync + episode)", len(res.FactIDs))
	}
	if len(res.EpisodeFactIDs) != 1 {
		t.Errorf("EpisodeFactIDs len = %d, want 1", len(res.EpisodeFactIDs))
	}
	if res.AsyncRequestID == "" || !res.SemanticPending {
		t.Errorf("async correlation missing: %+v", res)
	}
	facts, _ := store.List(context.Background(), asyncTestScope(), port.ListQuery{IncludeSuperseded: true})
	var sync, episode int
	for _, f := range facts {
		if f.Kind == FactEpisode {
			episode++
			continue
		}
		sync++
	}
	if sync != 1 {
		t.Errorf("sync semantic facts = %d, want 1", sync)
	}
	if episode != 1 {
		t.Errorf("episode facts = %d, want 1", episode)
	}
	if d := queueDepth(t, queue); d != 1 {
		t.Errorf("queue depth = %d, want 1", d)
	}
}

// TestSave_AsyncOutboxFailure_CompensatesEpisode pins §5.2 partial-
// failure semantics. When write_semantic_outbox fails the framework
// must reverse-walk and undo evidence projection + episode append so
// the ledger does not carry an unqueueable episode.
func TestSave_AsyncOutboxFailure_CompensatesEpisode(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(&failingQueue{err: errors.New("outbox down")}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "hi"}},
	})
	if err == nil {
		t.Fatal("expected outbox failure to surface")
	}
	facts, _ := store.List(context.Background(), asyncTestScope(), port.ListQuery{IncludeSuperseded: true})
	for _, f := range facts {
		if f.Kind == FactEpisode {
			t.Errorf("compensation failed to delete episode fact: %+v", f)
		}
	}
}

// TestSave_AsyncMixedOutboxFailure_DoesNotHidePartialCommit documents
// the atomicity expectation for mixed async requests. If the episode
// lane cannot write its durable outbox record, Save must not return an
// error while leaving the structured Facts leg committed invisibly.
func TestSave_AsyncMixedOutboxFailure_DoesNotHidePartialCommit(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(&failingQueue{err: errors.New("outbox down")}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Facts: []TemporalFact{{Kind: FactNote, Content: "structured fact"}},
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "raw turn"}},
	})
	if err == nil {
		t.Fatal("expected outbox failure to surface")
	}
	facts, err := store.List(context.Background(), asyncTestScope(), port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 0 {
		t.Fatalf("failed mixed async save must not leave hidden partial commits, got %+v", facts)
	}
}

// structuredAppendFailStore wraps MemoryStore and rejects Append for any
// fact that is not KindEpisode — used to simulate structured-facts leg
// failure after the episode lane succeeds.
type structuredAppendFailStore struct {
	*temporalstore.MemoryStore
}

func (s *structuredAppendFailStore) Append(ctx context.Context, facts []domain.TemporalFact) error {
	for _, f := range facts {
		if f.Kind != FactEpisode {
			return errors.New("structured append blocked")
		}
	}
	return s.MemoryStore.Append(ctx, facts)
}

// TestSave_AsyncMixedFactsLegFailure_CompensatesEpisodeAndOutbox pins
// atomicity when structured facts fail before write_semantic_outbox:
// no job is enqueued; episode facts and evidence mirrors roll back.
func TestSave_AsyncMixedFactsLegFailure_CompensatesEpisodeAndOutbox(t *testing.T) {
	store := &structuredAppendFailStore{MemoryStore: temporalstore.NewMemoryStore()}
	queue := asyncsemantic.New()
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Facts: []TemporalFact{{Kind: FactNote, Content: "structured fact"}},
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "raw turn"}},
	})
	if err == nil {
		t.Fatal("expected structured append failure to surface")
	}
	facts, err := store.List(context.Background(), asyncTestScope(), port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 0 {
		t.Fatalf("compensation must leave store empty, got %+v", facts)
	}
	if d := queueDepth(t, queue); d != 0 {
		t.Fatalf("compensation must cancel outbox job, queue depth = %d", d)
	}
}

// TestSave_DirectEpisodeFactRejected pins the public API boundary:
// KindEpisode is generated by the async episode lane only. Caller-
// supplied episode facts must not enter the sync semantic pipeline,
// where they would hit retrieval/entity projections via ProjectRequired.
func TestSave_DirectEpisodeFactRejected(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store))
	if err != nil {
		t.Fatal(err)
	}
	_, err = mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Facts: []TemporalFact{{Kind: FactEpisode, Content: "raw episode should not be accepted"}},
	})
	if err == nil {
		t.Fatal("expected direct KindEpisode save to be rejected")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("direct KindEpisode error = %v, want validation classification", err)
	}
	facts, err := store.List(context.Background(), asyncTestScope(), port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 0 {
		t.Fatalf("direct KindEpisode rejection must not write facts, got %+v", facts)
	}
}

// TestSave_AsyncInvalidScopeRunsValidateStage pins the async episode
// runner shape from the design doc: validate must run before
// build_episode / append_episode so malformed scope errors are
// attributed consistently and custom stores are not relied on for
// facade-level validation.
func TestSave_AsyncInvalidScopeRunsValidateStage(t *testing.T) {
	queue := asyncsemantic.New()
	mem, err := New(WithAsyncSemanticQueue(queue))
	if err != nil {
		t.Fatal(err)
	}
	_, trace, err := mem.(SaveExplainer).SaveExplain(context.Background(), Scope{}, SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "hi"}},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("invalid scope error = %v, want validation classification", err)
	}
	if len(trace.Stages) == 0 {
		t.Fatal("expected validate diagnostic in trace")
	}
	if trace.Stages[0].Stage != "validate" {
		t.Fatalf("first async stage = %q, want validate; full trace=%+v", trace.Stages[0].Stage, trace.Stages)
	}
	if trace.Stages[0].Status != "failed" {
		t.Fatalf("validate status = %q, want failed", trace.Stages[0].Status)
	}
}

// TestSave_AsyncKindEpisodeNotInRetrievalProjection pins §3.2: the
// raw episode fact MUST NOT bleed into retrieval results so a Recall
// after async save returns no hits for the episode body.
func TestSave_AsyncKindEpisodeNotInRetrievalProjection(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := asyncTestScope()
	_, err = mem.Save(context.Background(), scope, SaveRequest{
		Mode: WriteModeAsyncSemantic,
		Turns: []TurnContext{
			{ID: "t1", Speaker: "Alice", Text: "I love Paris"},
		},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	hits, err := mem.Recall(context.Background(), scope, Query{Text: "Paris", Limit: 10})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, h := range hits {
		if h.Fact.Kind == FactEpisode {
			t.Errorf("Recall must not surface KindEpisode: %+v", h)
		}
	}
}

// TestSave_StageDiagnostic_AsyncRequestIDCorrelation pins §7.1: every
// stage of the episode lane MUST carry the same AsyncRequestID so
// telemetry sinks can join sync ack and async completion.
func TestSave_StageDiagnostic_AsyncRequestIDCorrelation(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	hook := &captureHook{}
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
		WithTelemetryHook(hook),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mem.Save(context.Background(), asyncTestScope(), SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.AsyncRequestID == "" {
		t.Fatal("AsyncRequestID empty in result")
	}
	required := map[string]bool{
		"build_episode":            false,
		"append_episode":           false,
		"project_episode_evidence": false,
		"write_semantic_outbox":    false,
	}
	for _, ev := range hook.stages {
		if _, ok := required[ev.Stage]; !ok {
			continue
		}
		if ev.AsyncRequestID != res.AsyncRequestID {
			t.Errorf("stage %q AsyncRequestID = %q, want %q",
				ev.Stage, ev.AsyncRequestID, res.AsyncRequestID)
		}
		required[ev.Stage] = true
	}
	for stage, seen := range required {
		if !seen {
			t.Errorf("telemetry missing stage %q", stage)
		}
	}
}

// TestSave_AsyncResultIncludesEnqueuedRequestID is the explain
// variant: SaveExplain must mirror Save's correlation fields so
// callers walking trace + result see one consistent AsyncRequestID.
func TestSave_AsyncResultIncludesEnqueuedRequestID(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, trace, err := mem.(SaveExplainer).SaveExplain(context.Background(), asyncTestScope(), SaveRequest{
		Mode:  WriteModeAsyncSemantic,
		Turns: []TurnContext{{ID: "t1", Speaker: "Alice", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("SaveExplain: %v", err)
	}
	if res.AsyncRequestID == "" {
		t.Fatal("AsyncRequestID empty")
	}
	if len(trace.Stages) == 0 {
		t.Fatal("trace missing")
	}
	var hits int
	for _, st := range trace.Stages {
		if strings.HasPrefix(st.Stage, "build_episode") ||
			strings.HasPrefix(st.Stage, "append_episode") ||
			strings.HasPrefix(st.Stage, "project_episode_evidence") ||
			strings.HasPrefix(st.Stage, "write_semantic_outbox") {
			if st.AsyncRequestID != res.AsyncRequestID {
				t.Errorf("trace stage %q AsyncRequestID = %q",
					st.Stage, st.AsyncRequestID)
			}
			hits++
		}
	}
	if hits < 4 {
		t.Errorf("expected at least 4 async lane stages in trace, got %d", hits)
	}
}
