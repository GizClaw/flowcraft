package claw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestAppendMemoryFactWritesRecallableFact(t *testing.T) {
	app := newMemoryEnabledTestClaw(t)
	defer app.Close()

	ctx := context.Background()
	res, err := app.AppendMemoryFact(ctx, MemoryFact{
		Kind:      MemoryFactEvent,
		Content:   "The user washed pet:pet-123 and the pet became cleaner.",
		Subject:   "pet:pet-123",
		Predicate: "pet_drive",
		Object:    "wash",
		Entities:  []string{"pet:pet-123"},
	})
	if err != nil {
		t.Fatalf("AppendMemoryFact: %v", err)
	}
	if len(res.FactIDs) != 1 || res.FactIDs[0] == "" {
		t.Fatalf("FactIDs = %+v, want one id", res.FactIDs)
	}

	hits, err := app.memory.mem.Recall(ctx, app.memory.scope, recall.Query{
		Text:      "pet became cleaner washed",
		Kinds:     []recall.FactKind{recall.FactEvent},
		Limit:     5,
		GraphHops: 0,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("AppendMemoryFact fact was not recallable")
	}
	if got := hits[0].Fact.Content; !strings.Contains(got, "pet became cleaner") {
		t.Fatalf("recall content = %q", got)
	}
}

func TestAppendMemoryFactSupportsPublicSemanticKinds(t *testing.T) {
	app := newMemoryEnabledTestClaw(t)
	defer app.Close()

	ctx := context.Background()
	for _, kind := range []MemoryFactKind{
		MemoryFactEvent,
		MemoryFactState,
		MemoryFactPreference,
		MemoryFactProcedure,
		MemoryFactRelation,
		MemoryFactPlan,
		MemoryFactNote,
	} {
		res, err := app.AppendMemoryFact(ctx, MemoryFact{
			Kind:    kind,
			Content: "public semantic memory fact " + string(kind),
		})
		if err != nil {
			t.Fatalf("AppendMemoryFact kind=%s: %v", kind, err)
		}
		if len(res.FactIDs) != 1 {
			t.Fatalf("AppendMemoryFact kind=%s FactIDs = %+v, want one id", kind, res.FactIDs)
		}
		stored, err := app.memory.backend.TemporalStore().Get(ctx, app.memory.scope, res.FactIDs[0])
		if err != nil {
			t.Fatalf("Get kind=%s: %v", kind, err)
		}
		if stored.Kind != recall.FactKind(kind) {
			t.Fatalf("stored kind = %s, want %s", stored.Kind, kind)
		}
	}
}

func TestAppendMemoryFactDefaultsEmptyKindToNote(t *testing.T) {
	app := newMemoryEnabledTestClaw(t)
	defer app.Close()

	res, err := app.AppendMemoryFact(context.Background(), MemoryFact{
		Content: "empty kind should default to a note",
	})
	if err != nil {
		t.Fatalf("AppendMemoryFact: %v", err)
	}
	stored, err := app.memory.backend.TemporalStore().Get(context.Background(), app.memory.scope, res.FactIDs[0])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Kind != recall.FactNote {
		t.Fatalf("stored kind = %s, want note", stored.Kind)
	}
}

func TestAppendMemoryFactRejectsReservedEpisodeKind(t *testing.T) {
	app := newMemoryEnabledTestClaw(t)
	defer app.Close()

	_, err := app.AppendMemoryFact(context.Background(), MemoryFact{
		Kind:    MemoryFactKind(recall.FactEpisode),
		Content: "raw episode should not be accepted",
	})
	if err == nil {
		t.Fatal("expected reserved episode kind to fail")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("episode error = %v, want validation classification", err)
	}
}

func TestAppendMemoryFactPreservesStructuredFields(t *testing.T) {
	app := newMemoryEnabledTestClaw(t)
	defer app.Close()

	ctx := context.Background()
	observedAt := time.Date(2026, 7, 6, 8, 9, 10, 0, time.UTC)
	validFrom := observedAt.Add(-time.Hour)
	validTo := observedAt.Add(time.Hour)
	metadata := map[string]any{
		"type":          "pet_drive",
		"pet_id":        "pet-123",
		"points_delta":  float64(-3),
		"pet_exp_delta": float64(10),
		"life_delta":    map[string]any{"cleanliness": float64(15)},
		"tags":          []any{"pet", "care"},
	}
	res, err := app.AppendMemoryFact(ctx, MemoryFact{
		Kind:       MemoryFactEvent,
		Content:    "The user cared for the pet by washing it.",
		Subject:    " pet:pet-123 ",
		Predicate:  " pet_drive ",
		Object:     " wash ",
		Entities:   []string{"pet:pet-123", " ", "petdef:petdef-cat"},
		Metadata:   metadata,
		ObservedAt: observedAt,
		ValidFrom:  &validFrom,
		ValidTo:    &validTo,
	})
	if err != nil {
		t.Fatalf("AppendMemoryFact: %v", err)
	}
	stored, err := app.memory.backend.TemporalStore().Get(ctx, app.memory.scope, res.FactIDs[0])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Kind != recall.FactEvent ||
		stored.Subject != "pet:pet-123" ||
		stored.Predicate != "pet drive" ||
		stored.Object != "wash" {
		t.Fatalf("stored structured fields = kind=%s subject=%q predicate=%q object=%q",
			stored.Kind, stored.Subject, stored.Predicate, stored.Object)
	}
	for _, want := range []string{"pet:pet-123", "petdef:petdef-cat"} {
		if !hasString(stored.Entities, want) {
			t.Fatalf("entities = %q, missing %q", strings.Join(stored.Entities, ","), want)
		}
	}
	if !stored.ObservedAt.Equal(observedAt) ||
		stored.ValidFrom == nil || !stored.ValidFrom.Equal(validFrom) ||
		stored.ValidTo == nil || !stored.ValidTo.Equal(validTo) {
		t.Fatalf("stored times = observed=%s validFrom=%v validTo=%v", stored.ObservedAt, stored.ValidFrom, stored.ValidTo)
	}
	assertJSONEqual(t, stored.Metadata, metadata)
}

func TestAppendMemoryFactPreservesMetadataJSONRoundTrip(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(root)
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, enableTestMemory)

	ctx := context.Background()
	metadata := map[string]any{
		"source": "gizclaw",
		"score":  float64(3),
		"nested": map[string]any{"ok": true},
		"tags":   []any{"pet", "memory"},
	}
	res, err := app.AppendMemoryFact(ctx, MemoryFact{
		Kind:     MemoryFactNote,
		Content:  "metadata should survive persistence",
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("AppendMemoryFact: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ws2, err := workspace.NewLocalWorkspace(root)
	if err != nil {
		t.Fatalf("NewLocalWorkspace reopen: %v", err)
	}
	reopened, err := New(ws2)
	if err != nil {
		t.Fatalf("New reopen: %v", err)
	}
	defer reopened.Close()

	stored, err := reopened.memory.backend.TemporalStore().Get(ctx, reopened.memory.scope, res.FactIDs[0])
	if err != nil {
		t.Fatalf("Get reopened fact: %v", err)
	}
	assertJSONEqual(t, stored.Metadata, metadata)
}

func TestAppendMemoryFactMemoryDisabled(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, nil)
	defer app.Close()

	_, err = app.AppendMemoryFact(context.Background(), MemoryFact{Content: "memory is disabled"})
	if !errors.Is(err, ErrMemoryDisabled) {
		t.Fatalf("AppendMemoryFact error = %v, want ErrMemoryDisabled", err)
	}
}

func TestAppendMemoryFactAfterCloseReturnsError(t *testing.T) {
	app := newMemoryEnabledTestClaw(t)
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := app.AppendMemoryFact(context.Background(), MemoryFact{Content: "after close"})
	if !errors.Is(err, ErrMemoryDisabled) {
		t.Fatalf("AppendMemoryFact after close error = %v, want ErrMemoryDisabled", err)
	}
}

func TestAppendMemoryFactAfterCloseErrorReturnsDisabled(t *testing.T) {
	closeErr := errors.New("close failed")
	app := &Claw{
		memory: &memoryRuntime{
			mem: &closeErrMemory{err: closeErr},
		},
	}
	if err := app.CloseContext(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("CloseContext error = %v, want %v", err, closeErr)
	}
	_, err := app.AppendMemoryFact(context.Background(), MemoryFact{Content: "after failed close"})
	if !errors.Is(err, ErrMemoryDisabled) {
		t.Fatalf("AppendMemoryFact after failed close = %v, want ErrMemoryDisabled", err)
	}
}

func TestAppendMemoryFactValidation(t *testing.T) {
	app := newMemoryEnabledTestClaw(t)
	defer app.Close()

	tests := []struct {
		name string
		fact MemoryFact
	}{
		{
			name: "empty content",
			fact: MemoryFact{Kind: MemoryFactNote},
		},
		{
			name: "unsupported kind",
			fact: MemoryFact{Kind: "unknown", Content: "invalid kind"},
		},
		{
			name: "non json metadata",
			fact: MemoryFact{
				Kind:     MemoryFactNote,
				Content:  "invalid metadata",
				Metadata: map[string]any{"bad": func() {}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.AppendMemoryFact(context.Background(), tc.fact)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want validation classification", err)
			}
		})
	}
}

func TestAppendMemoryFactReturnsSaveError(t *testing.T) {
	saveErr := errors.New("save failed")
	app := &Claw{
		memory: &memoryRuntime{
			mem:   &saveErrMemory{err: saveErr},
			scope: recall.Scope{RuntimeID: "rt", UserID: "u", AgentID: "a"},
		},
	}
	_, err := app.AppendMemoryFact(context.Background(), MemoryFact{Content: "save should fail"})
	if !errors.Is(err, saveErr) {
		t.Fatalf("AppendMemoryFact error = %v, want %v", err, saveErr)
	}
}

func TestAppendMemoryFactReturnsSideEffectError(t *testing.T) {
	mem, err := recall.New()
	if err != nil {
		t.Fatalf("recall.New: %v", err)
	}
	defer mem.Close()

	sideErr := errors.New("side effect failed")
	app := &Claw{
		memory: &memoryRuntime{
			mem:   mem,
			side:  failingSideEffectProcessor{err: sideErr},
			scope: recall.Scope{RuntimeID: "rt", UserID: "u", AgentID: "a"},
		},
	}
	_, err = app.AppendMemoryFact(context.Background(), MemoryFact{Content: "side effects should fail"})
	if !errors.Is(err, sideErr) {
		t.Fatalf("AppendMemoryFact error = %v, want %v", err, sideErr)
	}
}

func TestAppendMemoryFactAsyncSemanticModeWritesFactSynchronously(t *testing.T) {
	app := newMemoryEnabledTestClawWith(t, func(cfg *Config) {
		cfg.Memory.Write.Mode = "async_semantic"
	})
	defer app.Close()

	res, err := app.AppendMemoryFact(context.Background(), MemoryFact{
		Kind:    MemoryFactEvent,
		Content: "facts-only async semantic append should write immediately",
	})
	if err != nil {
		t.Fatalf("AppendMemoryFact: %v", err)
	}
	if len(res.FactIDs) != 1 || res.AsyncRequestID != "" || res.SemanticPending {
		t.Fatalf("result = %+v, want one sync fact and no async pending", res)
	}
	if _, err := app.memory.backend.TemporalStore().Get(context.Background(), app.memory.scope, res.FactIDs[0]); err != nil {
		t.Fatalf("Get appended fact: %v", err)
	}
}

func TestAppendMemoryFactConcurrentClose(t *testing.T) {
	for i := 0; i < 20; i++ {
		app := newMemoryEnabledTestClaw(t)
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			_, err := app.AppendMemoryFact(context.Background(), MemoryFact{Content: "concurrent fact"})
			if err != nil && !errors.Is(err, ErrMemoryDisabled) {
				errs <- err
				return
			}
			errs <- nil
		}()
		go func() {
			<-start
			errs <- app.CloseContext(context.Background())
		}()
		close(start)
		for j := 0; j < 2; j++ {
			if err := <-errs; err != nil {
				t.Fatalf("iteration %d: %v", i, err)
			}
		}
	}
}

func TestAppendMemoryFactDrainsSideEffects(t *testing.T) {
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
	}
	res, err := runtime.appendMemoryFact(context.Background(), MemoryFact{
		Kind:    MemoryFactNote,
		Content: "direct fact should drain side effects",
	})
	if err != nil {
		t.Fatalf("appendMemoryFact: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Fatalf("FactIDs = %+v, want one id", res.FactIDs)
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
}

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

func newMemoryEnabledTestClaw(t *testing.T) *Claw {
	return newMemoryEnabledTestClawWith(t, nil)
}

func newMemoryEnabledTestClawWith(t *testing.T, mutate func(*Config)) *Claw {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	return newTestClaw(t, ws, staticLLM{reply: "ok"}, func(cfg *Config) {
		enableTestMemory(cfg)
		if mutate != nil {
			mutate(cfg)
		}
	})
}

func enableTestMemory(cfg *Config) {
	cfg.Memory.Enabled = true
	cfg.Memory.Retrieval.Backend = "memory"
	cfg.Memory.Backend = "memory"
}

func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("json = %s, want %s", gotJSON, wantJSON)
	}
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type closeErrMemory struct {
	recordingMemory
	err error
}

func (m *closeErrMemory) Close() error { return m.err }

type saveErrMemory struct {
	recordingMemory
	err error
}

func (m *saveErrMemory) Save(context.Context, recall.Scope, recall.SaveRequest) (recall.SaveResult, error) {
	return recall.SaveResult{}, m.err
}

type failingSideEffectProcessor struct {
	err error
}

func (p failingSideEffectProcessor) ProcessSideEffects(context.Context, recall.SideEffectProcessOptions) (recall.SideEffectProcessResult, error) {
	return recall.SideEffectProcessResult{}, p.err
}
