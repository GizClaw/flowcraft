package recall

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/asyncsemantic"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

// TestSave_ConcurrentAsyncTurnsOnly hammers WriteModeAsyncSemantic on one
// scope from many goroutines. Scope write lock serializes pipeline runs;
// each Save must land one episode + one outbox job with no LLM calls.
func TestSave_ConcurrentAsyncTurnsOnly(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	llm := &stubLLM{}
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
		WithLLMExtractor(llm),
	)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 24
	scope := asyncTestScope()
	ctx := context.Background()

	var wg sync.WaitGroup
	var okCount atomic.Int32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := mem.Save(ctx, scope, SaveRequest{
				Mode: WriteModeAsyncSemantic,
				Turns: []TurnContext{{
					ID:      fmt.Sprintf("turn-%d", i),
					Speaker: "Alice",
					Text:    fmt.Sprintf("message %d", i),
				}},
			})
			if err == nil {
				okCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := int(okCount.Load()); got != workers {
		t.Fatalf("successful saves = %d, want %d", got, workers)
	}
	if calls := llm.calls.Load(); calls != 0 {
		t.Fatalf("LLM calls = %d, want 0 on async Save path", calls)
	}

	facts, err := store.List(ctx, scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var episodes int
	for _, f := range facts {
		if f.Kind == FactEpisode {
			episodes++
		}
	}
	if episodes != workers {
		t.Fatalf("episode facts in store = %d, want %d", episodes, workers)
	}

	jobs, err := claimBatch(ctx, queue, "drain", testingNow(), workers+10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != workers {
		t.Fatalf("queued jobs = %d, want %d", len(jobs), workers)
	}
}

// TestSave_ConcurrentAsyncMixedFacts stresses mixed Facts+Turns async
// saves on one scope. Each goroutine must atomically commit both legs
// or roll back entirely (no orphaned episodes or structured facts).
func TestSave_ConcurrentAsyncMixedFacts(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	queue := asyncsemantic.New()
	mem, err := New(
		withTemporalStore(store),
		WithAsyncSemanticQueue(queue),
	)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 16
	scope := asyncTestScope()
	ctx := context.Background()

	var wg sync.WaitGroup
	var okCount atomic.Int32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := mem.Save(ctx, scope, SaveRequest{
				Mode: WriteModeAsyncSemantic,
				Facts: []TemporalFact{{
					Kind:    FactNote,
					Content: fmt.Sprintf("structured %d", i),
				}},
				Turns: []TurnContext{{
					ID:      fmt.Sprintf("turn-%d", i),
					Speaker: "Bob",
					Text:    fmt.Sprintf("raw %d", i),
				}},
			})
			if err == nil {
				okCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := int(okCount.Load()); got != workers {
		t.Fatalf("successful saves = %d, want %d", got, workers)
	}

	facts, err := store.List(ctx, scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var episodes, structured int
	for _, f := range facts {
		switch f.Kind {
		case FactEpisode:
			episodes++
		case FactNote:
			structured++
		}
	}
	if episodes != workers {
		t.Fatalf("episodes = %d, want %d", episodes, workers)
	}
	if structured != workers {
		t.Fatalf("structured facts = %d, want %d", structured, workers)
	}
	if d := queueDepth(t, queue); d != workers {
		t.Fatalf("queue depth = %d, want %d", d, workers)
	}
}

// testingNow isolates time for race tests from wall clock jitter.
func testingNow() time.Time {
	return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
}
