package ranker

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// fakeRerankLLM is a minimal llm.LLM satisfier for reranker tests.
// It returns a single canned JSON body per call and records the
// last prompt so tests can assert prompt-shape invariants.
type fakeRerankLLM struct {
	Body   string
	Err    error
	Prompt string
	Calls  int
}

func (f *fakeRerankLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	f.Calls++
	if len(msgs) > 0 && len(msgs[0].Parts) > 0 {
		f.Prompt = msgs[0].Parts[0].Text
	}
	if f.Err != nil {
		return llm.Message{}, llm.TokenUsage{}, f.Err
	}
	return llm.NewTextMessage(llm.RoleAssistant, f.Body), llm.TokenUsage{}, nil
}

func (f *fakeRerankLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("fakeRerankLLM: streaming not implemented")
}

func makeHits() []domain.Hit {
	return []domain.Hit{
		{Fact: domain.TemporalFact{ID: "a", Content: "Paris is the capital of France."}, Score: 0.9},
		{Fact: domain.TemporalFact{ID: "b", Content: "Bananas grow on trees in tropical climates."}, Score: 0.8},
		{Fact: domain.TemporalFact{ID: "c", Content: "France borders Belgium and Germany."}, Score: 0.7},
	}
}

// TestLLMReranker_ReordersByScore verifies the rerank stage actually
// applies the model's scores: candidate B (irrelevant) starts at
// rank 2 and should sink below A and C after rerank, even though B's
// pre-rerank score was higher than C's. This is the only invariant
// that matters for accuracy — without it the stage is a tax on
// latency.
func TestLLMReranker_ReordersByScore(t *testing.T) {
	client := &fakeRerankLLM{
		Body: `{"ranking":[{"index":0,"score":0.95},{"index":1,"score":0.05},{"index":2,"score":0.8}]}`,
	}
	r := NewLLM(client)
	out, err := r.Rerank(context.Background(), "What is the capital of France?", makeHits())
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("rerank must preserve cardinality, got %d", len(out))
	}
	if out[0].Fact.ID != "a" || out[1].Fact.ID != "c" || out[2].Fact.ID != "b" {
		t.Errorf("rerank order wrong, got %s,%s,%s", out[0].Fact.ID, out[1].Fact.ID, out[2].Fact.ID)
	}
	if client.Calls != 1 {
		t.Errorf("expected exactly 1 LLM call, got %d", client.Calls)
	}
}

// TestLLMReranker_FailureReturnsInputOrder pins the graceful-
// degradation contract: a provider error must not corrupt the input
// slice, so the caller can fall back to fusion order without
// inventing a synthetic ranking.
func TestLLMReranker_FailureReturnsInputOrder(t *testing.T) {
	client := &fakeRerankLLM{Err: errors.New("provider down")}
	r := NewLLM(client)
	in := makeHits()
	out, err := r.Rerank(context.Background(), "query", in)
	if err == nil {
		t.Fatal("expected error from rerank")
	}
	if len(out) != len(in) {
		t.Fatalf("rerank must return input slice on error, got len=%d want %d", len(out), len(in))
	}
	for i := range out {
		if out[i].Fact.ID != in[i].Fact.ID {
			t.Errorf("rerank reordered slice on error at idx %d: %s vs %s", i, out[i].Fact.ID, in[i].Fact.ID)
		}
	}
}

func TestLLMReranker_MalformedJSON_IsValidation(t *testing.T) {
	client := &fakeRerankLLM{Body: `{not json}`}
	r := NewLLM(client)
	_, err := r.Rerank(context.Background(), "query", makeHits())
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("malformed rerank JSON should map to Validation: %v", err)
	}
}

// TestLLMReranker_NilClientIsNoOp guards the opt-in safety wire: a
// caller building WithReranker(NewLLMReranker(nil)) from a missing
// CLI flag must not crash the Recall pipeline. Returning the input
// untouched makes the option safe to wire conditionally.
func TestLLMReranker_NilClientIsNoOp(t *testing.T) {
	var r *LLMReranker
	if _, err := r.Rerank(context.Background(), "q", makeHits()); err != nil {
		t.Errorf("nil receiver rerank should not error, got %v", err)
	}
	r = &LLMReranker{Client: nil}
	hits := makeHits()
	out, err := r.Rerank(context.Background(), "q", hits)
	if err != nil {
		t.Errorf("nil client rerank should not error, got %v", err)
	}
	if len(out) != len(hits) {
		t.Errorf("nil client rerank must echo input, got len=%d", len(out))
	}
}

// TestLLMReranker_PreservesTailBeyondMaxBatch pins the batch-cap
// contract: the reranker reorders the top MaxBatch hits and appends
// the un-reranked tail verbatim, so callers retain the full pool
// for downstream pagination without paying for an oversize prompt.
func TestLLMReranker_PreservesTailBeyondMaxBatch(t *testing.T) {
	hits := []domain.Hit{
		{Fact: domain.TemporalFact{ID: "a", Content: "x"}, Score: 0.9},
		{Fact: domain.TemporalFact{ID: "b", Content: "y"}, Score: 0.5},
		{Fact: domain.TemporalFact{ID: "c", Content: "z"}, Score: 0.3},
	}
	client := &fakeRerankLLM{
		Body: `{"ranking":[{"index":0,"score":0.1},{"index":1,"score":0.9}]}`,
	}
	r := &LLMReranker{Client: client, MaxBatch: 2}
	out, err := r.Rerank(context.Background(), "q", hits)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("rerank must preserve cardinality, got %d", len(out))
	}
	if out[len(out)-1].Fact.ID != "c" {
		t.Errorf("tail beyond MaxBatch must be appended verbatim, got tail=%s", out[len(out)-1].Fact.ID)
	}
}
