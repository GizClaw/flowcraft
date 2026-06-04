package ingest

import (
	"context"
	"fmt"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// LLMExtractor calls a sdk/llm.LLM for typed proposals, then deterministically
// grounds, arbitrates, and promotes accepted candidates into TemporalFact rows.
//
// The extractor uses the canonical FlowCraft LLM facade directly
// (rather than a recall-local "narrow port") so it inherits
// provider routing, structured-output, caps middleware, fallback,
// and telemetry from the same plumbing every other subsystem uses.
//
// Behaviour:
//   - Empty SourceEvidenceSpans or nil Client falls back to passthrough
//     (callers can prime extraction or skip it).
//   - Input.Facts are returned verbatim alongside any LLM-extracted
//     facts so callers can mix structured + free-form inputs.
//   - Each typed extractor call enforces its family schema via
//     llm.WithJSONSchema; providers that don't natively support
//     structured outputs get the schema through llm/with_caps downgrade
//     automatically.
//   - llm.ExtractJSON tolerates ```json fences and prose around the
//     JSON body so we do not have to engineer around imperfect
//     prompt adherence.
type LLMExtractor struct {
	// Client is the LLM facade. nil disables LLM extraction
	// entirely (extractor degrades to passthrough).
	Client llm.LLM
	// SchemaName labels the JSON schema in structured-output mode.
	// Defaults to "recall_semantic_proposals".
	SchemaName string
	// Temperature is passed via llm.WithTemperature when non-zero.
	Temperature float64
	// ExtraOptions lets callers append provider-specific options
	// such as model-specific reasoning or response controls.
	ExtraOptions []llm.GenerateOption
	// MaxConcurrentExtractors caps per-segment typed extractor calls.
	// Zero uses a conservative default to avoid provider rate-limit bursts.
	MaxConcurrentExtractors int
}

var _ port.Extractor = (*LLMExtractor)(nil)

// NewLLMExtractor wires an llm.LLM with the default system prompt.
func NewLLMExtractor(client llm.LLM) *LLMExtractor {
	return &LLMExtractor{
		Client:     client,
		SchemaName: "recall_semantic_proposals",
	}
}

// CompileExtraction implements Extractor.
//
// Path:
//  1. Caller-supplied Input.Facts pass through unchanged (clone).
//  2. Segment SourceEvidenceSpans, route each segment to proposal
//     families, run typed proposal extractors, then deterministically
//     ground, arbitrate, and promote accepted proposals into TemporalFacts.
//  3. Empty SourceEvidenceSpans / nil client -> no-op (passthrough only).
func (e *LLMExtractor) CompileExtraction(ctx context.Context, input port.IngestInput) (port.ExtractionResult, error) {
	out := make([]domain.TemporalFact, 0, len(input.Facts))
	for _, f := range input.Facts {
		out = append(out, f.Clone())
	}

	turnIndex, err := buildExtractorTurnIndex(input.Turns)
	if err != nil {
		return port.ExtractionResult{}, errdefs.Validation(fmt.Errorf("recall extractor: source turns: %w", err))
	}
	segments := segmentObservationSpans(input.SourceEvidenceSpans)
	if len(segments) == 0 || e.Client == nil {
		return port.ExtractionResult{PromotedFacts: out}, nil
	}
	routes, err := e.classifySegments(ctx, input, segments)
	if err != nil {
		return port.ExtractionResult{}, err
	}
	proposals, err := e.extractTypedProposals(ctx, input, routes)
	if err != nil {
		return port.ExtractionResult{}, err
	}
	grounding := groundProposals(ctx, proposals, input.SourceEvidenceSpans)
	arbitration := arbitrateGroundedProposals(grounding.Accepted)
	for _, loser := range arbitration.Losers {
		recordRejectedGroundedProposal(ctx, loser.Proposal, loser.Reason)
	}
	promotion := promoteProposals(ctx, arbitration.Winners)
	facts, compileDecisions := compileTemporalFacts(ctx, promotion.Accepted, turnIndex)
	out = append(out, facts...)
	return port.ExtractionResult{
		PromotedFacts:     out,
		ProposalLifecycle: buildProposalLifecycleDetail(proposals, grounding, arbitration, promotion, compileDecisions),
	}, nil
}
