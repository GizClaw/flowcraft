// Package runners defines the Runner interface that bench drivers consume.
//
// One Runner == one combination of (Memory backend, pipeline, extractor mode,
// embedder, …) — the entity for which we record qa.judge / latency / cost.
//
// New backends only need to implement Runner.New(); the eval / compare / ingest
// commands are backend-agnostic.
package runners

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// Scope identifies a memory partition for ingest and recall. Eval-owned so
// drivers are not locked to sdk/recall_v1 or sdk/recall types.
type Scope struct {
	RuntimeID string
	UserID    string
	AgentID   string
}

// RecallArtifact is the runner-neutral diagnostic projection of a backend's
// native recall result. It is used by reports, replay artifacts, and
// recall.k_hit, not as the answer model. Backends should render answers from
// their native shape via AnswerContext.
//
// EvidenceIDs are the raw evidence refs that can be matched against benchmark
// gold evidence ids, not necessarily only the source candidate's original
// matching ids.
type RecallArtifact struct {
	ID          string
	Content     string
	Score       float64
	Kind        string
	Sources     []string
	EvidenceIDs []string
	ValidFrom   string
	Metadata    map[string]any
}

// AnswerQuestion is the small part of a benchmark question a backend needs
// to render its own answer context. It deliberately avoids depending on the
// eval/dataset package so runner implementations can stay backend-owned.
type AnswerQuestion struct {
	Query   string
	AskedAt string
}

// AnswerContext is an answer-ready prompt body produced by a backend from its
// native recall results. Backends with structured memory should prefer this
// over flattening their hits into the runner-neutral Hit.Content field.
type AnswerContext struct {
	Body   string
	Format string
	// PromptTemplate is the backend-specific system prompt for answering
	// from Body. It is kept for API compatibility with older eval code, but
	// is no longer a fmt.Sprintf template and must not contain retrieved data.
	PromptTemplate string
}

type RecallStageAudit struct {
	Stages []RecallStageSnapshot `json:"stages,omitempty"`
}

type RecallStageSnapshot struct {
	Stage             string                    `json:"stage"`
	Source            string                    `json:"source,omitempty"`
	Status            string                    `json:"status,omitempty"`
	Added             int                       `json:"added,omitempty"`
	AddedFactIDs      []string                  `json:"added_fact_ids,omitempty"`
	ScannedLinks      int                       `json:"scanned_links,omitempty"`
	AddedFacts        int                       `json:"added_facts,omitempty"`
	AddedEvidenceRefs int                       `json:"added_evidence_refs,omitempty"`
	Candidates        []RecallCandidateSnapshot `json:"candidates,omitempty"`
}

type RecallCandidateSnapshot struct {
	FactID      string   `json:"fact_id,omitempty"`
	Source      string   `json:"source,omitempty"`
	Rank        int      `json:"rank,omitempty"`
	Score       float64  `json:"score,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	Sources     []string `json:"sources,omitempty"`
}

// Runner abstracts a Memory implementation under evaluation.
//
// Save's saveCount is the number of memory entries actually persisted by
// this call (not the number of input messages). For LLM-extractor runners
// it equals the count of facts the extractor produced; for raw runners it
// equals len(msgs minus empties). The driver logs this so we can spot
// "extractor returned 0 facts on conv-X" without an interactive debugger.
type Runner interface {
	Name() string
	Save(ctx context.Context, scope Scope, msgs []llm.Message) (saveCount int, saveLatency time.Duration, err error)
	Recall(ctx context.Context, scope Scope, query string, topK int) (artifacts []RecallArtifact, recallLatency time.Duration, err error)
	Close() error
}

// RecallStageAuditor is an optional Runner extension that returns the
// read pipeline's per-stage candidate snapshots for diagnostics.
type RecallStageAuditor interface {
	RecallWithStageAudit(ctx context.Context, scope Scope, query string, topK int) (artifacts []RecallArtifact, audit RecallStageAudit, recallLatency time.Duration, err error)
}

// AnswerContextRecaller lets a backend keep its native recall result shape for
// answer rendering. The returned artifacts are for diagnostics and report dumps;
// answer prompting should use AnswerContext.
type AnswerContextRecaller interface {
	RecallAnswerContext(ctx context.Context, scope Scope, question AnswerQuestion, topK int) (artifacts []RecallArtifact, answer AnswerContext, recallLatency time.Duration, err error)
}

// AnswerContextStageAuditor combines structured answer rendering with recall
// stage diagnostics.
type AnswerContextStageAuditor interface {
	RecallAnswerContextWithStageAudit(ctx context.Context, scope Scope, question AnswerQuestion, topK int) (artifacts []RecallArtifact, answer AnswerContext, audit RecallStageAudit, recallLatency time.Duration, err error)
}

// RawTurn carries a single conversation turn together with its upstream
// evidence id (e.g. LoCoMo dia_id). Backends that implement RawIngestSaver
// must persist EvidenceID as the entry's primary key so retrieval reports
// can score recall.k_hit against the dataset's evidence_ids.
type RawTurn struct {
	Role       string
	Content    string
	Speaker    string
	Timestamp  string
	EvidenceID string
	SessionID  string
	Images     []RawImage
}

// RawImage carries structured visual metadata attached to a RawTurn. It is
// intentionally separate from Content so runner adapters can decide how to
// expose visual evidence to their own extractor contracts.
type RawImage struct {
	URL     string
	Query   string
	Caption string
}

// RenderRawTurnContent renders a structured RawTurn for legacy/raw ingest paths
// that only accept plain text. Extractor-backed runners should prefer the
// structured fields directly.
func RenderRawTurnContent(t RawTurn) string {
	body := strings.TrimSpace(t.Content)
	speaker := strings.TrimSpace(t.Speaker)
	timestamp := strings.TrimSpace(t.Timestamp)
	if speaker == "" && timestamp == "" && len(t.Images) == 0 {
		return body
	}
	var b strings.Builder
	if timestamp != "" {
		b.WriteString("[")
		b.WriteString(timestamp)
		b.WriteString("]")
	}
	if speaker != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(speaker)
		b.WriteString(":")
	}
	if body != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(body)
	}
	for _, image := range t.Images {
		url := strings.TrimSpace(image.URL)
		query := strings.TrimSpace(image.Query)
		caption := strings.TrimSpace(image.Caption)
		if url == "" && query == "" && caption == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("ATTACHED_IMAGE_METADATA (visual evidence for this turn; not speaker-authored prose):")
		if query != "" {
			b.WriteString("\n- query: ")
			b.WriteString(query)
		}
		if caption != "" {
			b.WriteString("\n- caption: ")
			b.WriteString(caption)
		}
		if url != "" {
			b.WriteString("\n- url: ")
			b.WriteString(url)
		}
	}
	return strings.TrimSpace(b.String())
}

// RawIngestSaver is an optional Runner extension that ingests verbatim turns
// while preserving each turn's EvidenceID. Only used when the eval driver
// runs without an LLM extractor.
type RawIngestSaver interface {
	SaveRawTurns(ctx context.Context, scope Scope, turns []RawTurn) (saveCount int, saveLatency time.Duration, err error)
}

// SourceTurnSaver is an optional Runner extension for extractor-backed ingest
// that needs source metadata (EvidenceID / SessionID) in addition to text. It
// lets v2 pass typed RawTurns through SaveRequest.Turns so extracted facts
// can cite the original evidence ids.
type SourceTurnSaver interface {
	SaveSourceTurns(ctx context.Context, scope Scope, turns []RawTurn) (saveCount int, saveLatency time.Duration, err error)
}

// ContextualSourceTurnSaver is an optional online-ingest extension. The eval
// driver passes the current save point as turns and prior turns from the same
// dataset session as recentTurns; implementations may inject recentTurns as
// extract=false context and recall existing memories before saving turns.
type ContextualSourceTurnSaver interface {
	SaveSourceTurnsWithContext(ctx context.Context, scope Scope, turns []RawTurn, recentTurns []RawTurn) (saveCount int, saveLatency time.Duration, err error)
}
