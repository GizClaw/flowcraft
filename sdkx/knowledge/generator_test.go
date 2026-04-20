package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// stubLLM is a minimal llm.LLM used to drive generator tests deterministically.
// It returns canned responses keyed by which prompt template the request matches.
type stubLLM struct {
	abstractResp        string
	abstractErr         error
	overviewResp        string
	overviewErr         error
	datasetOverviewResp string
	calls               int
}

func (s *stubLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	s.calls++
	if len(msgs) == 0 {
		return llm.Message{}, llm.TokenUsage{}, errors.New("empty messages")
	}
	body := msgs[0].Content()
	switch {
	case strings.Contains(body, "Document summaries:"):
		if s.datasetOverviewResp == "" {
			return llm.Message{}, llm.TokenUsage{}, errors.New("no dataset overview canned")
		}
		return llm.NewTextMessage(llm.RoleAssistant, s.datasetOverviewResp), llm.TokenUsage{}, nil
	case strings.Contains(body, "structured overview"):
		if s.overviewErr != nil {
			return llm.Message{}, llm.TokenUsage{}, s.overviewErr
		}
		return llm.NewTextMessage(llm.RoleAssistant, s.overviewResp), llm.TokenUsage{}, nil
	default:
		if s.abstractErr != nil {
			return llm.Message{}, llm.TokenUsage{}, s.abstractErr
		}
		return llm.NewTextMessage(llm.RoleAssistant, s.abstractResp), llm.TokenUsage{}, nil
	}
}

func (s *stubLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("not implemented")
}

func TestGenerateDocumentContext_Success(t *testing.T) {
	stub := &stubLLM{abstractResp: "  short L0  ", overviewResp: "L1 body\n"}
	got, err := GenerateDocumentContext(context.Background(), stub, "doc body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Abstract != "short L0" {
		t.Fatalf("abstract should be trimmed: %q", got.Abstract)
	}
	if got.Overview != "L1 body" {
		t.Fatalf("overview should be trimmed: %q", got.Overview)
	}
	if stub.calls != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", stub.calls)
	}
}

func TestGenerateDocumentContext_AbstractError(t *testing.T) {
	stub := &stubLLM{abstractErr: errors.New("boom")}
	got, err := GenerateDocumentContext(context.Background(), stub, "body")
	if err == nil {
		t.Fatal("expected error")
	}
	if got != (DocumentContext{}) {
		t.Fatalf("expected zero context on abstract failure, got %+v", got)
	}
	if stub.calls != 1 {
		t.Fatalf("overview should be skipped, got %d calls", stub.calls)
	}
}

func TestGenerateDocumentContext_OverviewErrorPreservesAbstract(t *testing.T) {
	stub := &stubLLM{abstractResp: "kept", overviewErr: errors.New("nope")}
	got, err := GenerateDocumentContext(context.Background(), stub, "body")
	if err == nil {
		t.Fatal("expected error")
	}
	if got.Abstract != "kept" {
		t.Fatalf("abstract should be preserved on overview failure, got %q", got.Abstract)
	}
	if got.Overview != "" {
		t.Fatalf("overview should be empty, got %q", got.Overview)
	}
}

func TestGenerateDocumentContext_NilLLM(t *testing.T) {
	if _, err := GenerateDocumentContext(context.Background(), nil, "body"); err == nil {
		t.Fatal("expected error for nil llm")
	}
}

func TestGenerateDatasetContext_Success(t *testing.T) {
	stub := &stubLLM{abstractResp: "ds L0", datasetOverviewResp: "ds L1"}
	got, err := GenerateDatasetContext(context.Background(), stub, []DocumentSummary{
		{Name: "a.md", Abstract: "about a"},
		{Name: "b.md", Abstract: "about b"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Abstract != "ds L0" || got.Overview != "ds L1" {
		t.Fatalf("unexpected ctx: %+v", got)
	}
}

func TestGenerateDatasetContext_SkipsEmptyAbstracts(t *testing.T) {
	stub := &stubLLM{}
	got, err := GenerateDatasetContext(context.Background(), stub, []DocumentSummary{
		{Name: "a.md", Abstract: ""},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (DatasetContext{}) {
		t.Fatalf("expected zero context when no usable summaries, got %+v", got)
	}
	if stub.calls != 0 {
		t.Fatalf("expected no LLM calls, got %d", stub.calls)
	}
}

func TestGenerateDatasetContext_NilLLM(t *testing.T) {
	if _, err := GenerateDatasetContext(context.Background(), nil, nil); err == nil {
		t.Fatal("expected error for nil llm")
	}
}

func TestGenerateDatasetContext_OverviewError(t *testing.T) {
	stub := &stubLLM{abstractResp: "unused"}
	got, err := GenerateDatasetContext(context.Background(), stub, []DocumentSummary{
		{Name: "a.md", Abstract: "x"},
	})
	if err == nil {
		t.Fatal("expected error when dataset overview generation fails")
	}
	if got != (DatasetContext{}) {
		t.Fatalf("expected zero context on overview failure, got %+v", got)
	}
	if stub.calls != 1 {
		t.Fatalf("abstract step should be skipped, got %d calls", stub.calls)
	}
}

func TestGenerateDatasetContext_AbstractErrorPreservesOverview(t *testing.T) {
	stub := &stubLLM{datasetOverviewResp: "kept overview", abstractErr: errors.New("nope")}
	got, err := GenerateDatasetContext(context.Background(), stub, []DocumentSummary{
		{Name: "a.md", Abstract: "x"},
	})
	if err == nil {
		t.Fatal("expected error from abstract failure")
	}
	if got.Overview != "kept overview" {
		t.Fatalf("overview should be preserved on abstract failure, got %q", got.Overview)
	}
	if got.Abstract != "" {
		t.Fatalf("abstract should be empty, got %q", got.Abstract)
	}
}

// Verifies that the prompts shipped as exported constants are wired to the
// expected step. We assert by inspecting the prompt fed into the LLM.
func TestGenerateDocumentContext_UsesExportedPrompts(t *testing.T) {
	rec := &recordingLLM{resp: "ok"}
	if _, err := GenerateDocumentContext(context.Background(), rec, "BODY"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(rec.prompts))
	}
	if !strings.Contains(rec.prompts[0], "ONE sentence") || !strings.Contains(rec.prompts[0], "BODY") {
		t.Fatalf("first prompt should be AbstractPrompt with content, got: %q", rec.prompts[0])
	}
	if !strings.Contains(rec.prompts[1], "structured overview") || !strings.Contains(rec.prompts[1], "BODY") {
		t.Fatalf("second prompt should be OverviewPrompt with content, got: %q", rec.prompts[1])
	}
}

func TestGenerateDatasetContext_TruncatesOverviewIntoAbstract(t *testing.T) {
	rec := &recordingLLM{resp: "OK"}
	_, err := GenerateDatasetContext(context.Background(), rec, []DocumentSummary{
		{Name: "a.md", Abstract: "alpha"},
		{Name: "b.md", Abstract: ""},
		{Name: "c.md", Abstract: "gamma"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(rec.prompts))
	}
	if !strings.Contains(rec.prompts[0], "Document summaries:") {
		t.Fatalf("first prompt should be DatasetOverviewPrompt, got: %q", rec.prompts[0])
	}
	if !strings.Contains(rec.prompts[0], "- a.md: alpha") || !strings.Contains(rec.prompts[0], "- c.md: gamma") {
		t.Fatalf("dataset overview prompt should list usable summaries, got: %q", rec.prompts[0])
	}
	if strings.Contains(rec.prompts[0], "b.md") {
		t.Fatalf("dataset overview prompt should skip empty summaries, got: %q", rec.prompts[0])
	}
	// abstract prompt should distill the overview, not the original summaries
	if !strings.Contains(rec.prompts[1], "ONE sentence") || !strings.Contains(rec.prompts[1], "OK") {
		t.Fatalf("abstract prompt should re-use the generated overview, got: %q", rec.prompts[1])
	}
}

// recordingLLM captures every prompt it receives and returns the same
// canned response. Used to assert prompt routing.
type recordingLLM struct {
	resp    string
	prompts []string
}

func (r *recordingLLM) Generate(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if len(msgs) == 0 {
		return llm.Message{}, llm.TokenUsage{}, errors.New("empty messages")
	}
	r.prompts = append(r.prompts, msgs[0].Content())
	return llm.NewTextMessage(llm.RoleAssistant, r.resp), llm.TokenUsage{}, nil
}

func (r *recordingLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("not implemented")
}

func TestTruncateForPrompt(t *testing.T) {
	if got := truncateForPrompt("hello", 100); got != "hello" {
		t.Fatalf("short input should be unchanged, got %q", got)
	}
	got := truncateForPrompt("abcdefghij", 4)
	if !strings.HasPrefix(got, "abcd") || !strings.Contains(got, "(truncated)") {
		t.Fatalf("expected truncated marker, got %q", got)
	}
	if got := truncateForPrompt("abc", 0); got != "abc" {
		t.Fatalf("non-positive limit should be no-op, got %q", got)
	}
}
