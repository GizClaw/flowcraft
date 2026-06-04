// Package ingest owns the write-time ingestion pipeline. It turns raw
// conversational turns / caller-supplied facts into canonical TemporalFact
// ledger entries via a fixed stage chain (extract -> structurize -> normalize
// -> resolve entities -> resolve time -> policy -> merge_key -> id -> score).
package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/governance"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Stages assembles the canonical write-time pipeline. Each stage is pluggable
// so callers can swap in LLM-backed extractors / resolvers without churning the
// facade. The zero value of each interface is the deterministic implementation.
type Stages struct {
	Extractor         port.Extractor
	Structurizer      port.Structurizer
	Normalizer        port.Normalizer
	PredicateSynonyms port.PredicateSynonyms
	EntityResolver    port.EntityResolver
	AliasResolver     port.AliasResolver
	TimeResolver      port.TimeResolver
	SalienceScorer    port.SalienceScorer
	Policy            port.WritePolicy
	// Governance optionally replaces Policy with the full
	// sensitivity / retention / write chain. When set it takes
	// precedence over Policy for Apply decisions.
	Governance *governance.Governance
	IDGen      port.IDGenerator
	Clock      func() time.Time
}

// Default returns an Ingestor with deterministic stages wired up.
func Default() port.Ingestor {
	return New(Stages{})
}

// New constructs an Ingestor from explicit stages. Nil fields fall back to the
// deterministic implementation.
func New(s Stages) port.Ingestor {
	if s.Extractor == nil {
		s.Extractor = passthroughExtractor{}
	}
	if s.Structurizer == nil {
		s.Structurizer = DefaultStructurizer{}
	}
	if s.PredicateSynonyms == nil {
		s.PredicateSynonyms = NopPredicateSynonyms{}
	}
	if s.Normalizer == nil {
		s.Normalizer = newDefaultNormalizer(s.PredicateSynonyms)
	}
	if s.AliasResolver == nil {
		s.AliasResolver = NopAliasResolver{}
	}
	if s.EntityResolver == nil {
		s.EntityResolver = newAliasEntityResolver(s.AliasResolver)
	}
	if s.TimeResolver == nil {
		s.TimeResolver = passthroughTimeResolver{}
	}
	if s.SalienceScorer == nil {
		s.SalienceScorer = defaultSalienceScorer{}
	}
	if s.Policy == nil {
		s.Policy = governance.NopWritePolicy{}
	}
	if s.IDGen == nil {
		s.IDGen = newULIDGenerator()
	}
	if s.Clock == nil {
		s.Clock = time.Now
	}
	return &defaultIngestor{stages: s}
}

type defaultIngestor struct {
	stages Stages
}

// compile-time assertion: the concrete defaultIngestor must
// implement port.Ingestor.
var _ port.Ingestor = (*defaultIngestor)(nil)

func (c *defaultIngestor) Compile(ctx context.Context, input port.IngestInput) (port.IngestResult, error) {
	if input.Scope.RuntimeID == "" {
		return port.IngestResult{}, errdefs.Validationf("recall ingest: scope.runtime_id is required")
	}

	now := input.Now
	if now.IsZero() {
		now = c.stages.Clock()
	}
	// observedNow grounds relative-time resolution and ObservedAt.
	// It can differ from `now` when replaying historical
	// conversations — the caller still wants stable ULID generation
	// off the real wall clock, but "yesterday" must resolve against
	// the conversation's own wall time, not today's.
	observedNow := input.ObservedAt
	if observedNow.IsZero() {
		observedNow = now
	}

	usageAcc := newExtractorUsageAccumulator()
	guardAcc := newExtractorGuardAccumulator()
	extractCtx := withExtractorUsageAccumulator(ctx, usageAcc)
	extractCtx = withExtractorGuardAccumulator(extractCtx, guardAcc)
	extraction, err := c.stages.Extractor.CompileExtraction(extractCtx, input)
	if err != nil {
		return port.IngestResult{}, fmt.Errorf("recall ingest: extract: %w", err)
	}
	extracted := extraction.PromotedFacts

	var result port.IngestResult
	result.ExtractorTokenUsage = usageAcc.snapshot()
	result.ExtractorGuard = guardAcc.snapshot()
	result.ProposalLifecycle = extraction.ProposalLifecycle
	for i := range extracted {
		f := extracted[i]
		f.Scope = input.Scope
		if f.ObservedAt.IsZero() {
			f.ObservedAt = observedNow
		}
		// Structurizer runs BEFORE kind validation so promoted semantic
		// proposals get any remaining deterministic fields filled.
		// Caller-supplied fully-formed facts pass through unchanged
		// because the Structurizer only fills empty fields.
		before := f
		f = c.stages.Structurizer.Structurize(f, input)
		result.StructurizerCoverage.Add(DiffStructurizerCoverage(before, f))

		if !f.Kind.IsValid() {
			return port.IngestResult{}, errdefs.Validationf("recall ingest: fact %d has invalid kind %q", i, f.Kind)
		}

		f = c.stages.Normalizer.Normalize(f)
		f = c.stages.EntityResolver.Resolve(f)
		f = c.stages.TimeResolver.Resolve(f, observedNow)

		var allow bool
		if c.stages.Governance != nil {
			f, allow = c.stages.Governance.ApplyWrite(ctx, input.Scope, f, now)
			if !allow {
				result.Dropped = append(result.Dropped, diagnostic.DroppedFact{Fact: f, Reason: "governance:reject"})
				continue
			}
		} else {
			f, allow = c.stages.Policy.Apply(f)
			if !allow {
				result.Dropped = append(result.Dropped, diagnostic.DroppedFact{Fact: f, Reason: "policy:reject"})
				continue
			}
		}
		f.Scope = input.Scope
		if !f.Kind.IsValid() {
			return port.IngestResult{}, errdefs.Validationf("recall ingest: fact %d has invalid kind %q after policy", i, f.Kind)
		}

		if f.Kind == domain.KindParameter {
			var err error
			f, err = CanonicalizeParameterFact(f)
			if err != nil {
				return port.IngestResult{}, errdefs.Validationf("recall ingest: parameter fact %d: %v", i, err)
			}
			f.MergeKey = DefaultMergeKey(f)
		} else if f.MergeKey == "" {
			f.MergeKey = DefaultMergeKey(f)
		}

		if f.ID == "" {
			f.ID = c.stages.IDGen.NewID(f, now)
		}

		f = salienceScore(c.stages.SalienceScorer, f, input.Tier)

		result.Facts = append(result.Facts, f)
	}
	return result, nil
}

func salienceScore(scorer port.SalienceScorer, f domain.TemporalFact, tier string) domain.TemporalFact {
	if scorer == nil {
		scorer = defaultSalienceScorer{}
	}
	if ds, ok := scorer.(defaultSalienceScorer); ok {
		ds.tier = domain.NormalizeSaveTier(tier)
		return ds.Score(f)
	}
	return scorer.Score(f)
}

func mergeStrings(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range b {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
