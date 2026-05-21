package recall

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// QueryPlan is the planner output (reconstruct via diagnostics.Plan).
type QueryPlan = domain.QueryPlan

// Scope identifies the tenant/user partition for canonical memory. It
// aliases the internal canonical model so the public facade does not
// duplicate schema.
type Scope = domain.Scope

// FactKind classifies a canonical memory fact.
type FactKind = domain.FactKind

const (
	FactEvent      FactKind = domain.KindEvent
	FactState      FactKind = domain.KindState
	FactPreference FactKind = domain.KindPreference
	FactProcedure  FactKind = domain.KindProcedure
	FactRelation   FactKind = domain.KindRelation
	FactPlan       FactKind = domain.KindPlan
	FactNote       FactKind = domain.KindNote
	// FactEpisode is the raw-conversation FactKind written by the
	// synchronous episode lane of WriteModeAsyncSemantic. Episode
	// facts are excluded from default Recall and from every
	// projection except evidence — they represent durable source
	// turns, not semantic conclusions.
	FactEpisode FactKind = domain.KindEpisode
)

// EvidenceRef points back to source material used to produce a fact.
type EvidenceRef = domain.EvidenceRef

// MergeHints are LLM-supplied hints about merge behaviour. They are
// schema-level metadata only and do not participate in canonical
// merge decisions.
type MergeHints = domain.MergeHints

// TemporalFact is the public v2 memory unit. It aliases the internal
// canonical model — sdk/recall owns the public name, internal/model
// owns the schema definition.
//
// Field documentation lives on domain.TemporalFact; the following
// notes (mirrored here per the D1 godoc convention) document
// behaviour callers must understand at the public surface:
//
//	// Supersedes lists canonical fact IDs this fact replaces. All listed
//	// prior facts will be closed (ValidTo = ObservedAt, CorrectedBy =
//	// new fact ID) atomically with the append. Multiple priors are
//	// supported for N:1 merge use cases (e.g. consolidating fine-grained
//	// facts into a summary). Empty IDs are rejected at validate.
//	Supersedes []string
type TemporalFact = domain.TemporalFact

// TrustContext carries read-time visibility constraints (Phase D.2).
type TrustContext = domain.TrustContext

// Message is a caller-supplied conversational turn for extract context.
type Message = domain.Message

// ForgetMode selects soft vs hard deletion (Phase D.8).
type ForgetMode = domain.ForgetMode

const (
	ForgetSoft = domain.ForgetSoft
	ForgetHard = domain.ForgetHard
)

// FactVersion is one row in a fact's supersede history (Phase D.6).
type FactVersion = domain.FactVersion

// TurnContext is the typed per-turn channel adapters use to feed
// the LLMExtractor. Each TurnContext carries an id, an optional
// absolute timestamp, the canonical speaker name, the conversational
// role, and the body text — the same information adapters used to
// bake into a prose "[<date>] <Speaker>:" prefix.
//
// Passing typed turns instead of prose lets the SDK render the LLM
// user message in a canonical JSONL shape (one source of truth) and
// lets the Structurizer use the typed Time/Speaker fields directly
// for valid_from resolution and Subject inference — the LLM stops
// doing regex archaeology on prose.
type TurnContext = domain.TurnContext

// EntitySnapshot is a hint about an entity the canonical projection
// has already seen in this scope. The compiler uses snapshots to
// deduplicate freshly-extracted entities against historical
// canonical forms and to seed the Structurizer's NER pass with
// high-confidence matches. Snapshots are a soft hint — missing /
// outdated entries only mean less canonicalization, not extraction
// failure.
type EntitySnapshot = port.EntitySnapshot

// SaveRequest is the v2 ingestion input. Aliases the canonical
// domain type so the recall facade and internal pipelines share one
// schema (Phase E.2: types.go is "全部 = domain.X 别名").
type SaveRequest = domain.SaveRequest

// WriteMode controls SaveRequest semantics. Zero value selects the
// existing fully-synchronous Save path; WriteModeAsyncSemantic
// stores raw episodes synchronously and queues semantic extraction
// for caller-driven workers (see
// internal-docs/recall-v2-async-semantic-write.md §3.1).
type WriteMode = domain.WriteMode

const (
	WriteModeSync          = domain.WriteModeSync
	WriteModeAsyncSemantic = domain.WriteModeAsyncSemantic
)

// FactOrigin / FactOriginKind expose the canonical idempotency
// identifier stamped on facts that participate in durable workflows
// (async semantic write). See §3.3.
type FactOrigin = domain.FactOrigin

// FactOriginKind classifies how a fact entered the ledger.
type FactOriginKind = domain.FactOriginKind

const (
	OriginKindUnspecified        = domain.OriginKindUnspecified
	OriginKindEpisode            = domain.OriginKindEpisode
	OriginKindSemanticDerivation = domain.OriginKindSemanticDerivation
)

// AsyncSemanticQueue is the durable outbox callers wire via
// WithAsyncSemanticQueue. See §4.2 for SLA and idempotency contract.
type AsyncSemanticQueue = port.AsyncSemanticQueue

// AsyncSemanticJob is one durable semantic extraction work item.
type AsyncSemanticJob = port.AsyncSemanticJob

// AsyncSemanticReceipt is the synchronous return value of Enqueue.
type AsyncSemanticReceipt = port.AsyncSemanticReceipt

// AsyncSemanticResult is the worker's success report.
type AsyncSemanticResult = port.AsyncSemanticResult

// AsyncSemanticFailure is the worker's failure report.
type AsyncSemanticFailure = port.AsyncSemanticFailure

// AsyncSemanticClaimOptions controls queue Claim batching and scope
// filters for multi-tenant workers.
type AsyncSemanticClaimOptions = port.AsyncSemanticClaimOptions

// AsyncSemanticStatsFilter scopes AsyncSemanticQueueStats.
type AsyncSemanticStatsFilter = port.AsyncSemanticStatsFilter

// AsyncSemanticStats is the operator-facing queue health snapshot.
type AsyncSemanticStats = port.AsyncSemanticStats

// SaveResult reports the canonical fact ids that were appended to the
// ledger by this Save call. Dedupe/policy drops are not listed here;
// telemetry surfaces them via the stage diagnostic hook.
//
// WriteModeAsyncSemantic adds three correlation fields:
//
//   - AsyncRequestID is the durable work-item key shared by the
//     synchronous episode lane and the eventual semantic worker run.
//     Empty in the sync write path.
//   - EpisodeFactIDs disambiguates raw episode IDs from synchronous
//     semantic IDs; it is a (possibly empty) subset of FactIDs.
//   - SemanticPending is true when semantic extraction was queued —
//     Recall MAY NOT see semantic facts until the worker drains.
//
// FactIDs continues to list every id Save durably appended in this
// call (sync semantic + raw episode in mixed mode), preserving the
// pre-F.1 contract.
type SaveResult struct {
	FactIDs []string

	AsyncRequestID  string
	EpisodeFactIDs  []string
	SemanticPending bool
}

// TimeRange bounds timeline recall. Aliases domain.TimeRange.
type TimeRange = domain.TimeRange

// Query is the v2 recall input shape. Structured hints activate
// optional sources (timeline / relation / profile) via the planner;
// omitting them preserves PR-3 retrieval+entity behaviour.
type Query struct {
	Text      string
	Entities  []string
	Limit     int
	Subject   string
	Predicate string
	Object    string
	Kinds     []FactKind
	TimeRange TimeRange
	// GraphHops bounds graph expansion when graph is enabled via
	// WithGraphEnabled. Zero uses the graph projection default.
	GraphHops int

	// Trust applies read-time visibility filtering. Nil disables the
	// trust_filter stage.
	Trust *TrustContext

	// IncludeRetired surfaces soft-closed and expired facts (D.8).
	IncludeRetired bool
}

// TimeRangeFrom is a convenience for building a half-open range.
func TimeRangeFrom(from, to time.Time) TimeRange {
	return TimeRange{From: from, To: to}
}

// Hit is one materialized recall winner. Aliases the canonical
// domain type so the facade, internal pipelines, and diagnostics
// share one schema (Phase E.2: "全部 = domain.X 别名").
type Hit = domain.Hit

// LineageRelation classifies a fact's relationship to the root in a
// lineage DAG returned by Memory.Lineage. It aliases the internal
// domain type so callers can switch on the same constants the
// traversal emits without an extra public/internal mapping.
type LineageRelation = domain.LineageRelation

// Lineage relation constants surfaced by Memory.Lineage. The empty
// string sentinel is reserved by the domain layer for "unknown"; the
// traversal never emits it, so pattern-matching on these five values
// is exhaustive.
const (
	LineageRelationRoot      = domain.LineageRelationRoot
	LineageRelationSupersede = domain.LineageRelationSupersede
	LineageRelationFork      = domain.LineageRelationFork
	LineageRelationContest   = domain.LineageRelationContest
	LineageRelationMerge     = domain.LineageRelationMerge
)

// FactLineageNode is one node in the fact lineage DAG returned by
// Memory.Lineage. Depth is the BFS distance from the root fact
// (root = 0); SourceFactID is the fact whose lookup discovered this
// node (empty on the root). Nodes are sorted by (Depth asc, FactID
// asc) so the traversal output is deterministic across stores.
type FactLineageNode struct {
	Fact         TemporalFact
	Relation     LineageRelation
	SourceFactID string
	Depth        int
}

// Reranker is the optional post-build_hits stage that reorders a
// Hit slice by a stronger relevance signal than the deterministic
// in-pipeline ranker alone (typically an LLM call or
// cross-encoder).
//
// It runs after materialize / rank-boost and before the final
// TotalCap is applied so the reranker sees the widest fused pool
// (typically 2× the requested topK). Errors are non-fatal: the
// caller falls back to the input order when Rerank returns a
// non-nil error.
//
// Reranking is intentionally NOT in the default pipeline: it costs
// a per-Recall round-trip to a model the SDK does not own. Opt in
// via WithReranker only when latency and cost budgets allow it.
type Reranker = port.Reranker
