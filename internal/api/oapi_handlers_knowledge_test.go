package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/api/oas"
	"github.com/GizClaw/flowcraft/internal/knowledgeproc"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/internal/store"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"
)

type kbStubLLM struct {
	abstractResp        string
	overviewResp        string
	datasetOverviewResp string
}

func (s *kbStubLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if len(msgs) == 0 {
		return llm.Message{}, llm.TokenUsage{}, errors.New("empty prompt")
	}
	body := msgs[0].Content()
	switch {
	case strings.Contains(body, "Document summaries:"):
		return llm.NewTextMessage(llm.RoleAssistant, s.datasetOverviewResp), llm.TokenUsage{}, nil
	case strings.Contains(body, "structured overview"):
		return llm.NewTextMessage(llm.RoleAssistant, s.overviewResp), llm.TokenUsage{}, nil
	default:
		return llm.NewTextMessage(llm.RoleAssistant, s.abstractResp), llm.TokenUsage{}, nil
	}
}

func (s *kbStubLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("not implemented")
}

func newKnowledgeHandler(t *testing.T, withWorker bool) (*oapiHandler, *store.SQLiteStore, *knowledgeproc.Worker, knowledge.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	s, err := store.NewSQLiteStore(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ws := workspace.NewMemWorkspace()
	fs := knowledge.NewFSStore(ws)
	if err := fs.BuildIndex(ctx); err != nil {
		t.Fatal(err)
	}
	cs := knowledge.NewCachedStore(fs)

	var worker *knowledgeproc.Worker
	if withWorker {
		stub := &kbStubLLM{abstractResp: "abs", overviewResp: "ov", datasetOverviewResp: "ds-ov"}
		worker = knowledgeproc.New(knowledgeproc.Deps{
			FSStore:        fs,
			CachedStore:    cs,
			AppStore:       s,
			LLM:            stub,
			Concurrency:    2,
			QueueSize:      8,
			JobTimeout:     2 * time.Second,
			RollupDebounce: 30 * time.Millisecond,
			RollupTimeout:  2 * time.Second,
		})
		worker.Start(ctx)
		t.Cleanup(worker.Stop)
	}

	srv := &Server{deps: ServerDeps{Platform: &platform.Platform{
		Store:           s,
		Knowledge:       cs,
		KnowledgeWorker: worker,
	}}}
	return newOAPIHandler(srv), s, worker, cs
}

func TestReprocessDocument_FlipsToPendingAndSubmits(t *testing.T) {
	h, s, _, _ := newKnowledgeHandler(t, true)
	ctx := context.Background()

	if _, err := s.CreateDataset(ctx, &model.Dataset{ID: "ds", Name: "test"}); err != nil {
		t.Fatalf("create ds: %v", err)
	}
	doc, err := s.AddDocument(ctx, "ds", "doc.md", "body")
	if err != nil {
		t.Fatalf("add doc: %v", err)
	}
	failed := model.ProcessingFailed
	if err := s.UpdateDocumentStats(ctx, "ds", doc.ID, model.DocumentStatsPatch{ProcessingStatus: &failed}); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	resp, err := h.ReprocessDocument(ctx, oas.ReprocessDocumentParams{ID: "ds", DocId: doc.ID})
	if err != nil {
		t.Fatalf("reprocess: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	got, ok := resp.ProcessingStatus.Get()
	if !ok || got == "" {
		t.Fatal("processing_status should be set")
	}
	// Worker is enabled, so the response should reflect "processing"
	// (the inline post-submit promotion).
	if got != string(model.ProcessingRunning) {
		t.Fatalf("expected processing, got %s", got)
	}

	// Wait for the worker to settle the doc back to completed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fresh, _ := s.GetDocument(ctx, "ds", doc.ID)
		if fresh != nil && fresh.ProcessingStatus == model.ProcessingCompleted {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	fresh, _ := s.GetDocument(ctx, "ds", doc.ID)
	t.Fatalf("doc did not reach completed; status=%s", fresh.ProcessingStatus)
}

func TestReprocessDocument_NoWorker_MarksCompleted(t *testing.T) {
	h, s, _, _ := newKnowledgeHandler(t, false)
	ctx := context.Background()

	if _, err := s.CreateDataset(ctx, &model.Dataset{ID: "ds", Name: "test"}); err != nil {
		t.Fatalf("create ds: %v", err)
	}
	doc, err := s.AddDocument(ctx, "ds", "doc.md", "body")
	if err != nil {
		t.Fatalf("add doc: %v", err)
	}

	resp, err := h.ReprocessDocument(ctx, oas.ReprocessDocumentParams{ID: "ds", DocId: doc.ID})
	if err != nil {
		t.Fatalf("reprocess: %v", err)
	}
	got, _ := resp.ProcessingStatus.Get()
	if got != string(model.ProcessingCompleted) {
		t.Fatalf("expected completed, got %s", got)
	}
}
