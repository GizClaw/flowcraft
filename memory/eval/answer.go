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

// ResolvedSource is one canonical message a recalled item stands on: the text
// the store holds for it, and the dataset turn ingest recorded it came from.
type ResolvedSource struct {
	ConversationID string
	MessageID      string
	// TurnID is the dataset turn the message was ingested from (LoCoMo's
	// dia_id), empty when the store holds no ingest-side id for it.
	TurnID string
	Text   string
}

// ProvenanceResolver exposes the canonical messages behind a recalled item (a
// fact's source messages, for example). The harness does the matching itself, so
// hosts only have to resolve provenance: evidence recall is decided by turn
// identity when the store carries dataset turn ids, and by committed text
// otherwise.
type ProvenanceResolver interface {
	ResolveSources(ctx context.Context, item corememory.ContextItem) []ResolvedSource
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
