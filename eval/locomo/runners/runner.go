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

// Hit is one recall result in runner-neutral form.
type Hit struct {
	ID          string
	Content     string
	Score       float64
	Kind        string
	Sources     []string
	EvidenceIDs []string
	ValidFrom   string
	Metadata    map[string]any
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
	Recall(ctx context.Context, scope Scope, query string, topK int) (hits []Hit, recallLatency time.Duration, err error)
	Close() error
}

// RawTurn carries a single conversation turn together with its upstream
// evidence id (e.g. LoCoMo dia_id). Backends that implement RawIngestSaver
// must persist EvidenceID as the entry's primary key so retrieval reports
// can score recall.k_hit against the dataset's evidence_ids.
type RawTurn struct {
	Role       string
	Content    string
	EvidenceID string
	SessionID  string
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
