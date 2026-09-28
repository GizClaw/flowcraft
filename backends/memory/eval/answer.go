package eval

import (
	"context"
	"strings"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// Answerer turns one question plus the recalled context into a model answer.
// Implementations live outside this package so the harness keeps no inference
// dependency.
type Answerer interface {
	Answer(ctx context.Context, question Question, items []corememory.ContextItem) (string, error)
}

// Judge grades one generated answer against the question's expectations.
type Judge interface {
	Judge(ctx context.Context, question Question, answer string) (bool, error)
}

// Options configures the optional answering stage of a run.
type Options struct {
	// Answerer enables answer generation. Nil keeps the recall-only run.
	Answerer Answerer
	// Judge grades generated answers. Nil falls back to containment grading.
	Judge Judge
	// LenientJudge is an optional second grader (for example the LoCoMo
	// leaderboard-aligned prompt) evaluated over the same answers, so one run
	// can report strict and lenient rates side by side.
	LenientJudge Judge
	// Provenance resolves the canonical source texts behind a recalled item, so
	// derived items (facts, summaries) can satisfy evidence recall even though
	// their own text is a paraphrase. Nil keeps content matching only, which
	// under-reports recall for derived items.
	Provenance ProvenanceResolver
	// Concurrency bounds how many questions are processed in parallel. Zero or
	// one keeps the sequential pass. Parallelism changes only the schedule:
	// each question keeps its own request and prompts, and outcomes are merged
	// in question order, so reports match a sequential run for a deterministic
	// model. Runner, Answerer, Judge, and Provenance must be safe for
	// concurrent use when this is greater than one.
	Concurrency int
}

// ProvenanceResolver exposes the texts of the canonical sources a recalled
// item was derived from (a fact's source messages, for example). The harness
// does the matching itself, so hosts only have to resolve provenance.
type ProvenanceResolver interface {
	ResolveSourceTexts(ctx context.Context, item corememory.ContextItem) []string
}

// containsAll reports whether value contains every non-empty expectation,
// case-insensitively. It is the fallback grader used when a run has an
// Answerer but no Judge.
func containsAll(value string, wants []string) bool {
	haystack := strings.ToLower(value)
	graded := false
	for _, want := range wants {
		trimmed := strings.ToLower(strings.TrimSpace(want))
		if trimmed == "" {
			continue
		}
		graded = true
		if !strings.Contains(haystack, trimmed) {
			return false
		}
	}
	return graded
}
