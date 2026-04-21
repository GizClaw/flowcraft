package bootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/knowledgeproc"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/store"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"
)

// stubLLM mirrors the one in knowledgeproc tests but is duplicated here
// to avoid an internal-test cross-package dependency.
type stubLLM struct {
	abstractResp        string
	overviewResp        string
	datasetOverviewResp string
	delay               time.Duration
	calls               int32
}

func (s *stubLLM) Generate(ctx context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	atomic.AddInt32(&s.calls, 1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return llm.Message{}, llm.TokenUsage{}, ctx.Err()
		}
	}
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

func (s *stubLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("not implemented")
}

func newSeedHarness(t *testing.T, ll llm.LLM) (model.Store, knowledge.Store, *knowledgeproc.Worker, func()) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()

	appStore, err := store.NewSQLiteStore(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	ws := workspace.NewMemWorkspace()
	fs := knowledge.NewFSStore(ws)
	if err := fs.BuildIndex(ctx); err != nil {
		t.Fatalf("build index: %v", err)
	}
	cs := knowledge.NewCachedStore(fs)

	worker, err := knowledgeproc.New(knowledgeproc.Deps{
		FSStore:        fs,
		CachedStore:    cs,
		AppStore:       appStore,
		LLM:            ll,
		Concurrency:    2,
		QueueSize:      32,
		JobTimeout:     2 * time.Second,
		RollupDebounce: 30 * time.Millisecond,
		RollupTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	worker.Start(ctx)

	cleanup := func() {
		worker.Stop()
		_ = appStore.Close()
	}
	return appStore, cs, worker, cleanup
}

func TestInitCoPilotKnowledge_CreatesDatasetAndDocuments(t *testing.T) {
	stub := &stubLLM{
		abstractResp:        "doc-l0",
		overviewResp:        "doc-l1",
		datasetOverviewResp: "ds-l1",
	}
	appStore, ks, worker, cleanup := newSeedHarness(t, stub)
	defer cleanup()
	ctx := context.Background()

	initCoPilotKnowledge(ctx, appStore, ks, worker)

	ds, err := appStore.GetDataset(ctx, copilotKnowledgeDatasetID)
	if err != nil {
		t.Fatalf("dataset not created: %v", err)
	}
	if ds.Name == "" {
		t.Fatal("dataset name should be populated")
	}

	docs, err := appStore.ListDocuments(ctx, copilotKnowledgeDatasetID)
	if err != nil {
		t.Fatalf("list docs: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("expected at least one reference doc")
	}

	// Wait for worker to settle every doc to completed.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		docs, _ = appStore.ListDocuments(ctx, copilotKnowledgeDatasetID)
		for _, d := range docs {
			if d.ProcessingStatus != model.ProcessingCompleted {
				all = false
				break
			}
		}
		if all {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, d := range docs {
		if d.ProcessingStatus != model.ProcessingCompleted {
			t.Fatalf("doc %s status %s, want completed", d.Name, d.ProcessingStatus)
		}
		if d.L0Abstract != "doc-l0" {
			t.Fatalf("doc %s L0 = %q", d.Name, d.L0Abstract)
		}
		if d.L1Overview != "doc-l1" {
			t.Fatalf("doc %s L1 = %q", d.Name, d.L1Overview)
		}
	}

	// Wait for the debounced rollup to land the dataset abstract.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ds, _ = appStore.GetDataset(ctx, copilotKnowledgeDatasetID)
		if ds.L0Abstract != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ds.L0Abstract == "" {
		t.Fatal("dataset L0 should be populated by rollup")
	}
}

func TestRecoverPendingKnowledgeDocs_ResubmitsUnfinished(t *testing.T) {
	stub := &stubLLM{
		abstractResp:        "doc-l0",
		overviewResp:        "doc-l1",
		datasetOverviewResp: "ds-l1",
	}
	appStore, _, worker, cleanup := newSeedHarness(t, stub)
	defer cleanup()
	ctx := context.Background()

	if _, err := appStore.CreateDataset(ctx, &model.Dataset{ID: "ds-recover", Name: "Recover Test"}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}

	pending, err := appStore.AddDocument(ctx, "ds-recover", "pending.md", "pending body")
	if err != nil {
		t.Fatalf("add pending: %v", err)
	}
	processing, err := appStore.AddDocument(ctx, "ds-recover", "processing.md", "processing body")
	if err != nil {
		t.Fatalf("add processing: %v", err)
	}
	failed, err := appStore.AddDocument(ctx, "ds-recover", "failed.md", "failed body")
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}

	// Simulate post-crash state: pending stays pending, processing was
	// mid-flight, failed must NOT be picked up by recovery.
	failedStatus := model.ProcessingFailed
	if err := appStore.UpdateDocumentStats(ctx, "ds-recover", failed.ID, model.DocumentStatsPatch{ProcessingStatus: &failedStatus}); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	processingStatus := model.ProcessingRunning
	if err := appStore.UpdateDocumentStats(ctx, "ds-recover", processing.ID, model.DocumentStatsPatch{ProcessingStatus: &processingStatus}); err != nil {
		t.Fatalf("set processing: %v", err)
	}

	recoverPendingKnowledgeDocs(ctx, appStore, worker)

	// Wait for worker to drain.
	want := map[string]model.ProcessingStatus{
		pending.ID:    model.ProcessingCompleted,
		processing.ID: model.ProcessingCompleted,
		failed.ID:     model.ProcessingFailed,
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		docs, _ := appStore.ListDocuments(ctx, "ds-recover")
		ok := true
		for _, d := range docs {
			if want[d.ID] != d.ProcessingStatus {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	docs, _ := appStore.ListDocuments(ctx, "ds-recover")
	for _, d := range docs {
		t.Logf("doc %s: status=%s", d.Name, d.ProcessingStatus)
	}
	t.Fatal("recovery did not converge")
}
