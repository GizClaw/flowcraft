package recall

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func slowIngestor(delay time.Duration, calls *atomic.Int64) port.Ingestor {
	return ingest.New(ingest.Stages{Extractor: &slowExtractor{delay: delay, calls: calls}})
}

// slowExtractor simulates an LLM-backed ingest path for latency benches.
type slowExtractor struct {
	delay time.Duration
	calls *atomic.Int64
}

func (s *slowExtractor) Extract(ctx context.Context, input port.IngestInput) ([]domain.TemporalFact, error) {
	if s.calls != nil {
		s.calls.Add(1)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
	}
	return turnNoteExtractor{}.Extract(ctx, input)
}

func p95(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	slices.Sort(sorted)
	idx := (len(sorted)*95 + 99) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func assertAsyncSaveP95BeatsSync(
	t *testing.T,
	delay time.Duration,
	samples int,
	syncMem, asyncMem Memory,
	proc AsyncSemanticProcessor,
	scope Scope,
	turns []TurnContext,
	ingestCalls *atomic.Int64,
	resetCalls func(),
) {
	t.Helper()
	ctx := context.Background()
	var syncLatencies, asyncLatencies []time.Duration
	for i := range samples {
		resetCalls()
		start := time.Now()
		if _, err := syncMem.Save(ctx, scope, SaveRequest{Turns: turns}); err != nil {
			t.Fatalf("sync Save[%d]: %v", i, err)
		}
		syncLatencies = append(syncLatencies, time.Since(start))
		if ingestCalls.Load() == 0 {
			t.Fatalf("sync Save[%d] must invoke ingest/LLM", i)
		}

		resetCalls()
		start = time.Now()
		if _, err := asyncMem.Save(ctx, scope, SaveRequest{
			Mode:  WriteModeAsyncSemantic,
			Turns: turns,
		}); err != nil {
			t.Fatalf("async Save[%d]: %v", i, err)
		}
		asyncLatencies = append(asyncLatencies, time.Since(start))
		if ingestCalls.Load() != 0 {
			t.Fatalf("async Save[%d] must not invoke ingest/LLM on user path", i)
		}
	}

	syncP95 := p95(syncLatencies)
	asyncP95 := p95(asyncLatencies)
	if asyncP95 >= syncP95/10 {
		t.Fatalf("async p95 %v must be < sync p95/10 (%v); sync=%v async=%v",
			asyncP95, syncP95/10, syncLatencies, asyncLatencies)
	}

	resetCalls()
	if _, err := proc.ProcessAsyncSemantic(ctx, AsyncSemanticProcessOptions{
		Limit: samples,
		Scope: scope,
		Now:   time.Now().Add(25 * time.Hour),
	}); err != nil {
		t.Fatalf("ProcessAsyncSemantic: %v", err)
	}
	if ingestCalls.Load() != int64(samples) {
		t.Fatalf("processor ingest/LLM calls = %d, want %d", ingestCalls.Load(), samples)
	}
}

// TestAsyncWrite_SaveP95BeatsSync pins F.1c: user-facing Save on the
// async lane must not wait on slow ingest; processor drain handles LLM.
func TestAsyncWrite_SaveP95BeatsSync(t *testing.T) {
	const (
		samples = 5
		delay   = 300 * time.Millisecond
	)
	scope := asyncTestScope()
	turns := []TurnContext{{ID: "t1", Text: "latency bench turn"}}

	var ingestCalls atomic.Int64
	queue := NewInMemoryAsyncSemanticQueue()

	syncMem, err := New(withCompiler(slowIngestor(delay, &ingestCalls)))
	if err != nil {
		t.Fatalf("sync New: %v", err)
	}
	asyncMem, err := New(
		WithAsyncSemanticQueue(queue),
		withCompiler(slowIngestor(delay, &ingestCalls)),
	)
	if err != nil {
		t.Fatalf("async New: %v", err)
	}
	proc, ok := NewAsyncSemanticProcessor(asyncMem)
	if !ok {
		t.Fatal("processor missing")
	}
	assertAsyncSaveP95BeatsSync(t, delay, samples, syncMem, asyncMem, proc, scope, turns,
		&ingestCalls, func() { ingestCalls.Store(0) })
}

// TestAsyncWrite_SaveP95BeatsSync_SlowLLM repeats the F.1c contract on
// the public WithLLMExtractor path (2s mock provider). Skipped under
// -short so CI stays fast; run without -short before release.
func TestAsyncWrite_SaveP95BeatsSync_SlowLLM(t *testing.T) {
	if testing.Short() {
		t.Skip("slow LLM latency bench")
	}
	const (
		samples = 3
		delay   = 2 * time.Second
	)
	scope := asyncTestScope()
	turns := []TurnContext{{ID: "t1", Text: "async write bench"}}

	llmClient := &slowLLM{delay: delay}
	queue := NewInMemoryAsyncSemanticQueue()

	syncMem, err := New(WithLLMExtractor(llmClient))
	if err != nil {
		t.Fatalf("sync New: %v", err)
	}
	asyncMem, err := New(
		WithAsyncSemanticQueue(queue),
		WithLLMExtractor(llmClient),
	)
	if err != nil {
		t.Fatalf("async New: %v", err)
	}
	proc, ok := NewAsyncSemanticProcessor(asyncMem)
	if !ok {
		t.Fatal("processor missing")
	}
	assertAsyncSaveP95BeatsSync(t, delay, samples, syncMem, asyncMem, proc, scope, turns,
		&llmClient.calls, func() { llmClient.calls.Store(0) })
}

// slowLLM simulates provider latency on the sync Save path while
// returning valid extractor JSON.
type slowLLM struct {
	delay time.Duration
	calls atomic.Int64
}

func (s *slowLLM) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	s.calls.Add(1)
	time.Sleep(s.delay)
	return llm.NewTextMessage(model.RoleAssistant, `{"memories":[{"text":"bench","kind":"note"}]}`), llm.TokenUsage{}, nil
}

func (s *slowLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("slowLLM: streaming not implemented")
}
