package claw

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestSaveTurnDrainsSideEffects(t *testing.T) {
	mem, err := recall.New()
	if err != nil {
		t.Fatalf("recall.New: %v", err)
	}
	defer mem.Close()

	side := &recordingSideEffectProcessor{}
	runtime := &memoryRuntime{
		mem:  mem,
		side: side,
		scope: recall.Scope{
			RuntimeID: "rt",
			UserID:    "u",
			AgentID:   "a",
		},
		cfg: MemoryConfig{
			Write: MemoryWriteConfig{SaveConversation: true},
		},
	}
	if err := runtime.saveTurn(context.Background(), "ctx", "hello", model.NewTextMessage(model.RoleAssistant, "hi"), nil); err != nil {
		t.Fatalf("saveTurn: %v", err)
	}
	if side.calls != 1 {
		t.Fatalf("side-effect drain calls = %d, want 1", side.calls)
	}
	if side.last.WorkerID != "claw" {
		t.Fatalf("worker id = %q, want claw", side.last.WorkerID)
	}
	if side.last.Scope.PartitionKey() != runtime.scope.PartitionKey() {
		t.Fatalf("scope = %+v, want %+v", side.last.Scope, runtime.scope)
	}
	if side.last.Limit <= 0 {
		t.Fatalf("limit = %d, want positive", side.last.Limit)
	}
}

func TestSaveTurnWritesConfiguredBoardFacts(t *testing.T) {
	mem, err := recall.New()
	if err != nil {
		t.Fatalf("recall.New: %v", err)
	}
	defer mem.Close()
	side, ok := recall.NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("recall side-effect processor unavailable")
	}

	scope := recall.Scope{RuntimeID: "rt", UserID: "u", AgentID: "a"}
	runtime := &memoryRuntime{
		mem:   mem,
		side:  side,
		scope: scope,
		cfg: MemoryConfig{
			Write: MemoryWriteConfig{
				BoardFacts: []MemoryWriteBoardFactConfig{
					{
						BoardVar:       "tmp_story_progress_line",
						Kind:           "state",
						Subject:        "story_progress",
						Predicate:      "progress",
						RequiredPrefix: "story_progress: progress:",
					},
				},
			},
		},
	}

	boardVars := map[string]any{
		"tmp_story_progress_line": "ignored prefix\nstory_progress: progress: current_arc=origin; current_point=石猴入洞; next_arc=origin; next_point=拜为美猴王",
	}
	if err := runtime.saveTurn(context.Background(), "ctx", "hello", model.NewTextMessage(model.RoleAssistant, "hi"), boardVars); err != nil {
		t.Fatalf("saveTurn: %v", err)
	}

	hits, err := mem.Recall(context.Background(), scope, recall.Query{
		Text:      "story_progress current_point next_point",
		Kinds:     []recall.FactKind{recall.FactState},
		Limit:     5,
		GraphHops: 0,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1: %+v", len(hits), hits)
	}
	got := hits[0].Fact
	if got.Kind != recall.FactState || got.Subject != "story_progress" || got.Predicate != "progress" {
		t.Fatalf("fact shape = kind=%s subject=%q predicate=%q", got.Kind, got.Subject, got.Predicate)
	}
	if !strings.HasPrefix(got.Content, "story_progress: progress:") || !strings.Contains(got.Content, "current_point=石猴入洞") {
		t.Fatalf("fact content = %q", got.Content)
	}
}

func TestMemoryExtractSystemPromptIncludesLayout(t *testing.T) {
	cfg := MemoryConfig{
		Extract: MemoryExtractConfig{
			SystemPrompt: "Extract durable story memories.",
		},
		Layout: MemoryLayoutConfig{
			Lanes: []MemoryLaneConfig{
				{
					Name:        "story_progress",
					Kind:        "state",
					Description: "Current story position.",
					Extract:     "Capture the last completed scene and next unresolved scene.",
					Recall:      "Used to resume the story after interruptions.",
				},
			},
		},
	}

	got := cfg.extractSystemPrompt()
	for _, want := range []string{
		"Extract durable story memories.",
		"Memory layout:",
		"story_progress (kind: state)",
		"Current story position.",
		"<lane>: ...",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("extractSystemPrompt missing %q:\n%s", want, got)
		}
	}
}

func TestMemoryExtractStageTimeoutDefaultAndOverride(t *testing.T) {
	cfg := MemoryConfig{}.normalized("agent")
	if cfg.Extract.StageTimeout != "15s" {
		t.Fatalf("default stage timeout = %q, want 15s", cfg.Extract.StageTimeout)
	}
	got, err := cfg.Extract.stageTimeoutDuration()
	if err != nil {
		t.Fatalf("parse default stage timeout: %v", err)
	}
	if got != 15*time.Second {
		t.Fatalf("default stage timeout duration = %s, want 15s", got)
	}

	cfg.Extract.StageTimeout = "2m"
	got, err = cfg.Extract.stageTimeoutDuration()
	if err != nil {
		t.Fatalf("parse override stage timeout: %v", err)
	}
	if got != 2*time.Minute {
		t.Fatalf("override stage timeout duration = %s, want 2m", got)
	}
}

func TestMemoryDrainPendingProcessesAsyncSemantic(t *testing.T) {
	mem, err := recall.New(recall.WithAsyncSemanticQueue(recall.NewInMemoryAsyncSemanticQueue()))
	if err != nil {
		t.Fatalf("recall.New: %v", err)
	}
	defer mem.Close()
	async, ok := recall.NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("NewAsyncSemanticProcessor missing")
	}
	scope := recall.Scope{RuntimeID: "rt", UserID: "u", AgentID: "a"}
	ctx := context.Background()

	_, err = mem.Save(ctx, scope, recall.SaveRequest{
		Mode: recall.WriteModeAsyncSemantic,
		Turns: []recall.TurnContext{{
			ID:      "turn-1",
			Speaker: "user",
			Text:    "remember this turn",
		}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	obs, ok := mem.(recall.AsyncSemanticQueueObserver)
	if !ok {
		t.Fatal("AsyncSemanticQueueObserver missing")
	}
	stats, err := obs.AsyncSemanticQueueStats(ctx, scope)
	if err != nil {
		t.Fatalf("AsyncSemanticQueueStats: %v", err)
	}
	if stats.Pending != 1 {
		t.Fatalf("pending before drain = %d, want 1", stats.Pending)
	}

	runtime := &memoryRuntime{
		mem:   mem,
		async: async,
		scope: scope,
	}
	if err := runtime.drainPending(ctx); err != nil {
		t.Fatalf("drainPending: %v", err)
	}
	stats, err = obs.AsyncSemanticQueueStats(ctx, scope)
	if err != nil {
		t.Fatalf("AsyncSemanticQueueStats after drain: %v", err)
	}
	if stats.Pending != 0 || stats.Completed != 1 {
		t.Fatalf("stats after drain = %+v, want pending=0 completed=1", stats)
	}
}

func TestMemoryCloseDrainsPendingAsyncSemantic(t *testing.T) {
	mem, err := recall.New(recall.WithAsyncSemanticQueue(recall.NewInMemoryAsyncSemanticQueue()))
	if err != nil {
		t.Fatalf("recall.New: %v", err)
	}
	async, ok := recall.NewAsyncSemanticProcessor(mem)
	if !ok {
		t.Fatal("NewAsyncSemanticProcessor missing")
	}
	scope := recall.Scope{RuntimeID: "rt", UserID: "u", AgentID: "a"}
	ctx := context.Background()

	_, err = mem.Save(ctx, scope, recall.SaveRequest{
		Mode: recall.WriteModeAsyncSemantic,
		Turns: []recall.TurnContext{{
			ID:      "turn-1",
			Speaker: "user",
			Text:    "remember this turn",
		}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	obs, ok := mem.(recall.AsyncSemanticQueueObserver)
	if !ok {
		t.Fatal("AsyncSemanticQueueObserver missing")
	}
	stats, err := obs.AsyncSemanticQueueStats(ctx, scope)
	if err != nil {
		t.Fatalf("AsyncSemanticQueueStats: %v", err)
	}
	if stats.Pending != 1 {
		t.Fatalf("pending before close = %d, want 1", stats.Pending)
	}

	runtime := &memoryRuntime{
		mem:   mem,
		async: async,
		scope: scope,
		cfg:   MemoryConfig{Write: MemoryWriteConfig{Mode: "async_semantic"}},
	}
	start := time.Now()
	if err := runtime.close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("close elapsed = %s, want under 1s", elapsed)
	}
	stats, err = obs.AsyncSemanticQueueStats(ctx, scope)
	if err != nil {
		t.Fatalf("AsyncSemanticQueueStats after close: %v", err)
	}
	if stats.Pending != 0 || stats.Completed != 1 {
		t.Fatalf("stats after close = %+v, want pending=0 completed=1", stats)
	}
}

type recordingSideEffectProcessor struct {
	calls int
	last  recall.SideEffectProcessOptions
}

func (p *recordingSideEffectProcessor) ProcessSideEffects(_ context.Context, opts recall.SideEffectProcessOptions) (recall.SideEffectProcessResult, error) {
	p.calls++
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	p.last = opts
	return recall.SideEffectProcessResult{Claimed: 1, Completed: 1}, nil
}
