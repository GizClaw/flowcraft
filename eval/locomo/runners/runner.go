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
	ScoreLabel  string
	FinalScore  float64
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
	Body         string
	Format       string
	SystemPrompt string
}

type RecallStageAudit struct {
	Stages []RecallStageSnapshot `json:"stages,omitempty"`
}

type RecallStageSnapshot struct {
	Stage             string                      `json:"stage"`
	Source            string                      `json:"source,omitempty"`
	Status            string                      `json:"status,omitempty"`
	Query             *RecallQueryIntent          `json:"query_intent,omitempty"`
	ActivatedLenses   []RecallActivatedLens       `json:"activated_lenses,omitempty"`
	TaskIntents       []string                    `json:"task_intents,omitempty"`
	TotalBudget       int                         `json:"total_budget,omitempty"`
	Suggested         int                         `json:"suggested,omitempty"`
	SuggestedByTask   map[string]int              `json:"suggested_by_task,omitempty"`
	SuggestedFactIDs  []string                    `json:"suggested_fact_ids,omitempty"`
	InputCount        int                         `json:"input_count,omitempty"`
	OutputCount       int                         `json:"output_count,omitempty"`
	Dropped           int                         `json:"dropped,omitempty"`
	DropReasons       map[string]int              `json:"drop_reasons,omitempty"`
	Added             int                         `json:"added,omitempty"`
	AddedFactIDs      []string                    `json:"added_fact_ids,omitempty"`
	ScannedLinks      int                         `json:"scanned_links,omitempty"`
	AddedFacts        int                         `json:"added_facts,omitempty"`
	AddedEvidenceRefs int                         `json:"added_evidence_refs,omitempty"`
	CoverageBundles   []RecallCoverageBundle      `json:"coverage_bundles,omitempty"`
	ScoreSummary      *RecallAssessmentSummary    `json:"score_summary,omitempty"`
	Candidates        []RecallCandidateSnapshot   `json:"candidates,omitempty"`
	Assessment        []RecallAssessmentComponent `json:"assessment,omitempty"`
	PackTrace         []RecallCandidateSnapshot   `json:"pack_trace,omitempty"`
}

type RecallQueryIntent struct {
	QueryLen                      int                          `json:"query_len,omitempty"`
	Entities                      []string                     `json:"entities,omitempty"`
	Kinds                         []string                     `json:"kinds,omitempty"`
	Subject                       string                       `json:"subject,omitempty"`
	Predicate                     string                       `json:"predicate,omitempty"`
	Object                        string                       `json:"object,omitempty"`
	HasTimeRange                  bool                         `json:"has_time_range,omitempty"`
	HasExplicitDate               bool                         `json:"has_explicit_date,omitempty"`
	HasRelativeTemporalExpression bool                         `json:"has_relative_temporal_expression,omitempty"`
	TokenCount                    int                          `json:"token_count,omitempty"`
	NumericCount                  int                          `json:"numeric_count,omitempty"`
	QuotedCount                   int                          `json:"quoted_count,omitempty"`
	ProperCount                   int                          `json:"proper_count,omitempty"`
	Strategy                      string                       `json:"strategy,omitempty"`
	Confidence                    float64                      `json:"confidence,omitempty"`
	Alternates                    []RecallIntentRouteCandidate `json:"alternates,omitempty"`
	Signals                       []string                     `json:"signals,omitempty"`
	FallbackReason                string                       `json:"fallback_reason,omitempty"`
}

type RecallIntentRouteCandidate struct {
	Strategy   string  `json:"strategy,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

type RecallActivatedLens struct {
	Lens        string  `json:"lens,omitempty"`
	Weight      float64 `json:"weight,omitempty"`
	Budget      int     `json:"budget,omitempty"`
	ActivatedBy string  `json:"activated_by,omitempty"`
}

type RecallCoverageBundle struct {
	SeedFactID      string   `json:"seed_fact_id,omitempty"`
	RescuedFactIDs  []string `json:"rescued_fact_ids,omitempty"`
	ReplacedFactIDs []string `json:"replaced_fact_ids,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

type RecallCandidateSnapshot struct {
	FactID           string   `json:"fact_id,omitempty"`
	Source           string   `json:"source,omitempty"`
	Rank             int      `json:"rank,omitempty"`
	ScoreLabel       string   `json:"score_label,omitempty"`
	DiscoveryScore   float64  `json:"discovery_score,omitempty"`
	AssessmentScore  float64  `json:"assessment_relevance_score,omitempty"`
	RankScore        float64  `json:"rank_score,omitempty"`
	FinalScore       float64  `json:"final_score,omitempty"`
	EvidenceIDs      []string `json:"evidence_ids,omitempty"`
	Sources          []string `json:"sources,omitempty"`
	RankOutputRank   int      `json:"rank_output_rank,omitempty"`
	ContextPackRank  int      `json:"context_pack_rank,omitempty"`
	PrimarySource    string   `json:"primary_source,omitempty"`
	ProjectionRoutes []string `json:"projection_routes,omitempty"`
	DroppedReason    string   `json:"dropped_reason,omitempty"`
}

type RecallAssessmentComponent struct {
	ID                 string  `json:"id,omitempty"`
	Kind               string  `json:"kind,omitempty"`
	HardConstraintPass bool    `json:"hard_constraint_pass,omitempty"`
	SupportScore       float64 `json:"support_score,omitempty"`
	StructuredScore    float64 `json:"structured_score,omitempty"`
	LiteralScore       float64 `json:"literal_score,omitempty"`
	SemanticScore      float64 `json:"semantic_score,omitempty"`
	SourcePrior        float64 `json:"source_prior,omitempty"`
	RelevanceScore     float64 `json:"relevance_score,omitempty"`
	Confidence         float64 `json:"confidence,omitempty"`
	Reason             string  `json:"reason,omitempty"`
	DropReason         string  `json:"drop_reason,omitempty"`
	FallbackReason     string  `json:"fallback_reason,omitempty"`
	EquivalenceGroup   string  `json:"equivalence_group,omitempty"`
	SupportGroup       string  `json:"support_group,omitempty"`
	DiversityGroup     string  `json:"diversity_group,omitempty"`
}

type RecallAssessmentSummary struct {
	Count                int     `json:"count,omitempty"`
	RelevanceScoreMin    float64 `json:"relevance_score_min,omitempty"`
	RelevanceScoreMax    float64 `json:"relevance_score_max,omitempty"`
	RelevanceScoreAvg    float64 `json:"relevance_score_avg,omitempty"`
	SemanticScoreAvg     float64 `json:"semantic_score_avg,omitempty"`
	SupportScoreAvg      float64 `json:"support_score_avg,omitempty"`
	StructuredScoreAvg   float64 `json:"structured_score_avg,omitempty"`
	LiteralScoreAvg      float64 `json:"literal_score_avg,omitempty"`
	SourcePriorAvg       float64 `json:"source_prior_avg,omitempty"`
	ConfidenceAvg        float64 `json:"confidence_avg,omitempty"`
	HardConstraintPasses int     `json:"hard_constraint_passes,omitempty"`
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
	appendVisualEvidenceBlock(&b, t.Images)
	return strings.TrimSpace(b.String())
}

// RenderRawImageMetadata renders LoCoMo image fields into plain text for
// adapters that must pass visual metadata through text-only extractor APIs.
func RenderRawImageMetadata(images []RawImage) string {
	var b strings.Builder
	appendVisualEvidenceBlock(&b, images)
	return strings.TrimSpace(b.String())
}

func appendVisualEvidenceBlock(b *strings.Builder, images []RawImage) {
	for _, image := range images {
		url := strings.TrimSpace(image.URL)
		query := strings.TrimSpace(image.Query)
		caption := strings.TrimSpace(image.Caption)
		if url == "" && query == "" && caption == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("speaker_shared_image (image shared by the speaker in this turn; metadata is not quoted speech):")
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
