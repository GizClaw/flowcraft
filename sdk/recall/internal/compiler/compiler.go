package compiler

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/governance"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Input drives a single Compile call. Phase 1 keeps the shape narrow:
// callers supply structured facts plus optional raw context. Later
// phases will add multi-turn extractor inputs without breaking this
// interface.
type Input struct {
	Scope model.Scope
	// Facts are caller-supplied structured facts. PR-2 treats these
	// as authoritative content; the compiler still normalizes
	// scope/id/time/merge_key and runs deterministic policy hooks.
	Facts []model.TemporalFact
	// Text is free-form extractor input. The default passthrough
	// extractor ignores it; LLMExtractor consumes it when configured.
	Text string
	// Now is the wall clock used when filling missing ObservedAt /
	// generating IDs. Tests inject deterministic clocks here.
	Now time.Time
}

// Result is what Memory.Save persists.
type Result struct {
	Facts []model.TemporalFact
	// Dropped explains facts the compiler discarded before persistence.
	// Store-backed dedupe/supersede decisions are reported by the
	// conflict resolver in Memory.Save.
	Dropped []DroppedFact
}

// DroppedFact carries a structured reason for why a candidate fact
// did not enter the canonical ledger.
type DroppedFact struct {
	Fact   model.TemporalFact
	Reason string
}

// Compiler owns the write-time compilation pipeline. The interface is
// final shape; implementations grow richer in later phases without
// callers changing.
type Compiler interface {
	Compile(ctx context.Context, input Input) (Result, error)
}

// Stages assembles the canonical write-time pipeline. Each stage is
// pluggable so Phase 4 can swap in LLM-backed extractors / resolvers
// without churning the facade. The zero value of each interface is
// the deterministic Phase 1 implementation.
type Stages struct {
	Extractor         Extractor
	Normalizer        Normalizer
	PredicateSynonyms PredicateSynonyms
	EntityResolver    EntityResolver
	AliasResolver     AliasResolver
	TimeResolver      TimeResolver
	SalienceScorer    SalienceScorer
	Policy            Policy
	// Governance optionally replaces Policy with the full
	// sensitivity/retention/write chain. When set it takes
	// precedence over Policy for Apply decisions.
	Governance *governance.Governance
	IDGen      IDGenerator
	Clock      func() time.Time
}

// Default returns a Compiler with deterministic Phase 1 stages wired
// up. Callers in tests can override individual fields via With*
// helpers; PR-3+ will offer opt-in Extractor implementations.
func Default() Compiler {
	return New(Stages{})
}

// New constructs a Compiler from explicit stages. Nil fields fall
// back to the Phase 1 deterministic implementation.
func New(s Stages) Compiler {
	if s.Extractor == nil {
		s.Extractor = passthroughExtractor{}
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
	return &compiler{stages: s}
}

type compiler struct {
	stages Stages
}

func (c *compiler) Compile(ctx context.Context, input Input) (Result, error) {
	if input.Scope.RuntimeID == "" {
		return Result{}, fmt.Errorf("recall compiler: scope.runtime_id is required")
	}

	now := input.Now
	if now.IsZero() {
		now = c.stages.Clock()
	}

	extracted, err := c.stages.Extractor.Extract(ctx, input)
	if err != nil {
		return Result{}, fmt.Errorf("recall compiler: extract: %w", err)
	}

	var result Result
	for i := range extracted {
		f := extracted[i]
		f.Scope = input.Scope
		if f.ObservedAt.IsZero() {
			f.ObservedAt = now
		}
		if !f.Kind.IsValid() {
			return Result{}, fmt.Errorf("recall compiler: fact %d has invalid kind %q", i, f.Kind)
		}

		f = c.stages.Normalizer.Normalize(f)
		f = c.stages.EntityResolver.Resolve(f)
		f = c.stages.TimeResolver.Resolve(f, now)

		var allow bool
		if c.stages.Governance != nil {
			f, allow = c.stages.Governance.ApplyWrite(ctx, input.Scope, f, now)
			if !allow {
				result.Dropped = append(result.Dropped, DroppedFact{Fact: f, Reason: "governance:reject"})
				continue
			}
		} else {
			f, allow = c.stages.Policy.Apply(f)
			if !allow {
				result.Dropped = append(result.Dropped, DroppedFact{Fact: f, Reason: "policy:reject"})
				continue
			}
		}
		f.Scope = input.Scope
		if !f.Kind.IsValid() {
			return Result{}, fmt.Errorf("recall compiler: fact %d has invalid kind %q after policy", i, f.Kind)
		}

		if f.MergeKey == "" {
			f.MergeKey = DefaultMergeKey(f)
		}

		if f.ID == "" {
			f.ID = c.stages.IDGen.NewID(f, now)
		}

		f = c.stages.SalienceScorer.Score(f)

		result.Facts = append(result.Facts, f)
	}
	return result, nil
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
