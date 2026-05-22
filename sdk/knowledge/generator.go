package knowledge

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// Prompt templates used to derive layered context from raw documents.
// Exported so callers can override or compose their own pipelines while
// reusing the SDK's defaults for the common case.
const (
	// AbstractPrompt produces a single-sentence L0 summary (~100 tokens).
	AbstractPrompt = `Summarize the following document in ONE sentence (max 100 tokens).
Focus on: what it is, what it covers, who it's for.

Document:
%s

One-sentence summary:`

	// OverviewPrompt produces a structured L1 overview (~1000 tokens).
	OverviewPrompt = `Create a structured overview of this document (max 1000 tokens).
Include:
- Key topics covered
- Important concepts
- Navigation hints (which sections cover what)

Document:
%s

Overview:`

	// DatasetOverviewPrompt produces a dataset-level L1 from per-document
	// abstracts ("- name: abstract" lines).
	DatasetOverviewPrompt = `Create an overview for this document collection based on the following summaries.
Include: what the collection covers, key documents, how they relate.

Document summaries:
%s

Collection overview:`
)

// DefaultPromptInputLimit is the maximum number of characters of document
// content fed into a prompt. Content beyond this is truncated to keep
// prompts within typical context windows.
const DefaultPromptInputLimit = 8000

// DocumentContext groups the layered context for a single document.
type DocumentContext struct {
	Abstract string // L0
	Overview string // L1
}

// DatasetContext groups the layered context for an entire dataset.
type DatasetContext struct {
	Abstract string // dataset-level L0
	Overview string // dataset-level L1
}

// DocumentSummary pairs a document name with its L0 abstract, used as
// input to GenerateDatasetContext.
type DocumentSummary struct {
	Name     string
	Abstract string
}

// GenerateDocumentContext synthesizes L0 (abstract) and L1 (overview) for
// a document by issuing two LLM calls. Pure function: no I/O, no caching,
// no retries; callers own scheduling and persistence.
//
// Returns a partial result on error: if abstract generation fails the
// zero-value context is returned with the error; if overview fails the
// already-generated abstract is preserved so callers can choose to persist
// it.
func GenerateDocumentContext(ctx context.Context, l llm.LLM, content string) (DocumentContext, error) {
	if l == nil {
		return DocumentContext{}, errdefs.Validationf("knowledge: llm is required")
	}
	abstract, err := generate(ctx, l, fmt.Sprintf(AbstractPrompt, truncateForPrompt(content, DefaultPromptInputLimit)))
	if err != nil {
		return DocumentContext{}, fmt.Errorf("knowledge: generate abstract: %w", err)
	}
	overview, err := generate(ctx, l, fmt.Sprintf(OverviewPrompt, truncateForPrompt(content, DefaultPromptInputLimit)))
	if err != nil {
		return DocumentContext{Abstract: abstract}, fmt.Errorf("knowledge: generate overview: %w", err)
	}
	return DocumentContext{Abstract: abstract, Overview: overview}, nil
}

// GenerateDatasetContext derives dataset-level L0 + L1 from per-document
// abstracts. The L1 overview is generated first, then distilled into L0.
// Returns an empty context with no error when summaries is empty.
func GenerateDatasetContext(ctx context.Context, l llm.LLM, summaries []DocumentSummary) (DatasetContext, error) {
	if l == nil {
		return DatasetContext{}, errdefs.Validationf("knowledge: llm is required")
	}
	lines := make([]string, 0, len(summaries))
	for _, s := range summaries {
		if s.Abstract == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", s.Name, s.Abstract))
	}
	if len(lines) == 0 {
		return DatasetContext{}, nil
	}
	summariesText := strings.Join(lines, "\n")

	overview, err := generate(ctx, l, fmt.Sprintf(DatasetOverviewPrompt, summariesText))
	if err != nil {
		return DatasetContext{}, fmt.Errorf("knowledge: generate dataset overview: %w", err)
	}
	abstract, err := generate(ctx, l, fmt.Sprintf(AbstractPrompt, overview))
	if err != nil {
		return DatasetContext{Overview: overview}, fmt.Errorf("knowledge: generate dataset abstract: %w", err)
	}
	return DatasetContext{Abstract: abstract, Overview: overview}, nil
}

func generate(ctx context.Context, l llm.LLM, prompt string) (string, error) {
	resp, _, err := l.Generate(ctx, []llm.Message{
		llm.NewTextMessage(llm.RoleUser, prompt),
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content()), nil
}

func truncateForPrompt(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n...(truncated)"
}
