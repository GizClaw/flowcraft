// Package flowcraft is the default bench Runner: in-memory retrieval Index,
// pipeline.LTM, and the AdditiveExtractor — i.e. exactly what an out-of-the-box
// `recall.New(Config{Index: memidx.New(), LLM: …})` produces.
package flowcraft

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

// Options configures the Flowcraft default runner.
//
// All quality-impacting knobs (rerank / soft-merge / context recall / score
// threshold) are SDK-level toggles forwarded straight into recall.Config and
// pipeline.LTM — bench has no business owning that logic.
type Options struct {
	Name     string
	LLM      llm.LLM            // required iff Save uses the LLM extractor path
	Embedder embedding.Embedder // optional; enables vector lane

	MaxFactsPerCall  int
	IncludeAssistant bool
	ExtractPrompt    string

	// SaveWithContext forwards to recall.Config.SaveWithContext. When true, the
	// extractor receives existing-memory snippets via WithExistingFacts.
	SaveWithContext bool
	// SoftMerge defaults to true at the SDK layer; pass a pointer to override.
	SoftMerge *bool

	// RerankerLLM, when set, installs an LLMReranker against pipeline.LTM via
	// WithReranker. Empty disables rerank.
	RerankerLLM llm.LLM
	// ScoreThreshold forwards to pipeline.WithScoreThreshold. 0 keeps the
	// SDK default (0.05).
	ScoreThreshold float64

	// MultiRecall forwards to pipeline.WithMultiRecall. When true,
	// LTM uses three-lane recall (vector + bm25 + entity) + RRFFusion
	// instead of the legacy single-lane vector + boost topology.
	// Defaults to false to keep historical runs reproducible.
	MultiRecall bool

	// EntityStore forwards to recall.WithEntityStore. When true,
	// every Save Links its facts' entities into a sibling inverted
	// index, and the recall pipeline gains a 4th lane
	// (pipeline.ModeEntityLink) that materialises the linked entry
	// ids via DocGetter and fuses them with the vector / BM25 /
	// entity-filter lanes through RRF. Auto-enables multi-recall.
	EntityStore bool

	// EntityStoreMaxLinkedCount forwards to
	// recall.WithEntityStoreMaxLinkedCount. Sentinel semantics
	// (matches the SDK option):
	//
	//   - 0: do NOT forward — the SDK applies its safe default
	//     ([defaultEntityMaxLinkedCount] = 100). Use this when you
	//     have no specific opinion.
	//   - >0: exact threshold; forwarded verbatim.
	//   - <0: forwarded verbatim — explicit, audited gate-off
	//     opt-out. The SDK emits a one-time warning via the
	//     configured logger.
	//
	// No-op when EntityStore is false.
	EntityStoreMaxLinkedCount int

	// EntityLinkBoost forwards to pipeline.WithEntityLinkBoost. When
	// > 0 the entity-store contribution switches from "4th RRF lane"
	// to "post-fusion score boost" — vector + BM25 own candidate
	// generation, entity-link only re-ranks the fused result by
	// multiplying matched hits' scores by (1 + w). Multi-hop and
	// open-domain questions, which depend on candidate diversity,
	// no longer get starved by lane flooding. No-op when EntityStore
	// is false. See pipeline.WithEntityLinkBoost docstring for the
	// LoCoMo failure mode the boost mode mitigates.
	EntityLinkBoost float64

	// QueryEntityLLM forwards to recall.WithQueryEntityExtractor.
	// When non-nil, the retrieval pipeline replaces its rule-based
	// query-side entity extractor with an LLM call that mirrors the
	// write-side extractor's noun-phrase vocabulary, closing the
	// asymmetry between QueryEntities and the EntityStore keys. Cost:
	// ~1 LLM call per recall. Recommended to share the same alias as
	// ExtractorLLM so query / write sides agree on what counts as an
	// entity. No-op when EntityStore is false (no entity-link path
	// to exercise).
	QueryEntityLLM llm.LLM

	// UpdateResolverLLM, when set, installs an LLMUpdateResolver via
	// recall.WithUpdateResolver. The resolver runs once per Save call
	// after extraction and decides ADD / UPDATE / DELETE / NOOP for
	// each candidate memory against the new fact batch — equivalent
	// to mem0 v3's memory linker. Empty disables the resolver
	// (default).
	//
	// UpdateResolverTopK bounds the candidates fed to the resolver
	// LLM. 0 keeps the SDK default (20).
	UpdateResolverLLM  llm.LLM
	UpdateResolverTopK int

	// RecentTurnsK enables the cross-batch reference-resolution
	// context window backed by sdk/history.InMemoryStore: on every
	// Save the most recent K messages from PRIOR Save batches on
	// the same scope are read from the store and injected into the
	// extractor as ExtractOptions.RecentMessages so pronouns / short
	// references / chronology hints established in earlier batches
	// remain resolvable. 0 disables (default); typical: 10-20.
	//
	// Note: validated -1.75pp on LoCoMo10 (session-batched ingest);
	// the feature is architecturally targeted at multi-batch /
	// streaming ingest topologies where the current batch carries
	// minimal context.
	RecentTurnsK int

	// OnFactsExtracted is invoked synchronously after every
	// successful Save with the scope and the extractor's output
	// before any retrieval mutation (soft-merge / supersede /
	// MD5 dedup). It enables diagnostics that need to inspect what
	// the extractor actually produced for a given batch — e.g. the
	// --dump-facts probe in eval/locomo/cli.go used to distinguish
	// extract miss from recall miss without bolting yet another
	// abstraction onto recall.Memory.
	//
	// The callback runs in the caller's goroutine, so it MUST be
	// goroutine-safe when the eval's ingest_concurrency > 1.
	// nil disables the callback.
	OnFactsExtracted func(scope recall.Scope, facts []recall.ExtractedFact)
}

// Runner is the default bench Runner.
type Runner struct {
	name      string
	mem       recall.Memory
	onExtract func(scope recall.Scope, facts []recall.ExtractedFact)
}

// New returns a new bench runner. Caller must Close().
func New(opts Options) (runners.Runner, error) {
	if opts.Name == "" {
		opts.Name = "flowcraft-default"
	}
	maxFacts := opts.MaxFactsPerCall
	if maxFacts == 0 {
		maxFacts = 200 // LoCoMo conversations span hundreds of turns
	}
	idx := memidx.New()
	memOpts := []recall.Option{
		recall.WithLLM(opts.LLM),
		recall.WithEmbedder(opts.Embedder),
		recall.WithRequireUserID(),
		recall.WithMaxFactsPerCall(maxFacts),
		recall.WithIncludeAssistant(opts.IncludeAssistant),
	}
	if opts.SaveWithContext {
		memOpts = append(memOpts, recall.WithSaveContext(0, 0))
	}
	if opts.SoftMerge != nil && !*opts.SoftMerge {
		memOpts = append(memOpts, recall.WithoutSoftMerge())
	}
	if opts.RecentTurnsK > 0 {
		memOpts = append(memOpts, recall.WithRecentTurns(opts.RecentTurnsK))
	}
	if opts.UpdateResolverLLM != nil {
		topK := opts.UpdateResolverTopK
		if topK <= 0 {
			topK = 20
		}
		memOpts = append(memOpts, recall.WithUpdateResolver(
			&recall.LLMUpdateResolver{LLM: opts.UpdateResolverLLM},
			topK,
		))
	}
	if opts.ExtractPrompt != "" || opts.LLM != nil {
		memOpts = append(memOpts, recall.WithExtractor(&recall.AdditiveExtractor{
			LLM:              opts.LLM,
			IncludeAssistant: opts.IncludeAssistant,
			MaxFacts:         maxFacts,
			PromptTemplate:   opts.ExtractPrompt,
		}))
	}
	// LTM tuning is funneled through recall.WithLTMOption so feature
	// flags (e.g. recall.WithEntityStore) can layer their own auto-
	// wired LTM options on top without us having to remember to
	// thread them on the bench side. See recall.WithLTMOption.
	pipeOpts := []pipeline.LTMOption{}
	if opts.ScoreThreshold > 0 {
		pipeOpts = append(pipeOpts, pipeline.WithScoreThreshold(opts.ScoreThreshold))
	}
	if opts.RerankerLLM != nil {
		pipeOpts = append(pipeOpts, pipeline.WithReranker(&pipeline.LLMReranker{LLM: opts.RerankerLLM}))
	}
	if opts.MultiRecall {
		pipeOpts = append(pipeOpts, pipeline.WithMultiRecall(true))
	}
	if opts.EntityLinkBoost > 0 {
		pipeOpts = append(pipeOpts, pipeline.WithEntityLinkBoost(opts.EntityLinkBoost))
	}
	if len(pipeOpts) > 0 {
		memOpts = append(memOpts, recall.WithLTMOption(pipeOpts...))
	}
	if opts.EntityStore {
		// recall auto-wires the lookup stage + lane + resolver on
		// top of pipeOpts; no need to thread them here.
		memOpts = append(memOpts, recall.WithEntityStore(0))
		// Forward both positive (exact threshold) and negative
		// (explicit gate-off opt-out) values verbatim; only "0"
		// means "no opinion, let the SDK pick the safe default" —
		// it must NOT call WithEntityStoreMaxLinkedCount so the
		// explicit-tracking flag in cfg stays false and the safe
		// default applies. Aligns with the es-default semantics on
		// the SDK side.
		if opts.EntityStoreMaxLinkedCount != 0 {
			memOpts = append(memOpts, recall.WithEntityStoreMaxLinkedCount(opts.EntityStoreMaxLinkedCount))
		}
		if opts.QueryEntityLLM != nil {
			memOpts = append(memOpts, recall.WithQueryEntityExtractor(opts.QueryEntityLLM))
		}
	}

	mem, err := recall.New(idx, memOpts...)
	if err != nil {
		return nil, err
	}
	return &Runner{name: opts.Name, mem: mem, onExtract: opts.OnFactsExtracted}, nil
}

// Name implements runners.Runner.
func (r *Runner) Name() string { return r.name }

// Save implements runners.Runner.
func (r *Runner) Save(ctx context.Context, scope recall.Scope, msgs []llm.Message) (int, time.Duration, error) {
	t0 := time.Now()
	res, err := r.mem.Save(ctx, scope, msgs)
	if err != nil {
		return 0, time.Since(t0), err
	}
	if r.onExtract != nil && len(res.Facts) > 0 {
		r.onExtract(scope, res.Facts)
	}
	return len(res.EntryIDs), time.Since(t0), nil
}

// SaveRaw is a fallback path for runs without an LLM extractor; one MemoryEntry
// is created per non-empty user/assistant turn. Each entry's ID is
// auto-generated, so recall.k_hit cannot be evaluated through this path —
// callers that need evidence scoring should use SaveRawTurns instead.
func (r *Runner) SaveRaw(ctx context.Context, scope recall.Scope, msgs []llm.Message) (int, time.Duration, error) {
	t0 := time.Now()
	saved := 0
	for i, m := range msgs {
		txt := m.Content()
		if txt == "" {
			continue
		}
		entry := recall.Entry{
			Content:    txt,
			Categories: []string{"raw"},
			Source:     recall.Source{RuntimeID: scope.RuntimeID},
		}
		if _, err := r.mem.Add(ctx, scope, entry); err != nil {
			return saved, time.Since(t0), fmt.Errorf("add_raw turn %d: %w", i, err)
		}
		saved++
	}
	return saved, time.Since(t0), nil
}

// SaveRawTurns implements runners.RawIngestSaver: it preserves each turn's
// EvidenceID as the MemoryEntry primary key so recall.k_hit becomes
// meaningful. Empty IDs fall back to the auto-generated ULID.
func (r *Runner) SaveRawTurns(ctx context.Context, scope recall.Scope, turns []runners.RawTurn) (int, time.Duration, error) {
	t0 := time.Now()
	saved := 0
	for i, t := range turns {
		if t.Content == "" {
			continue
		}
		entry := recall.Entry{
			ID:         t.EvidenceID,
			Content:    t.Content,
			Categories: []string{"raw"},
			Source:     recall.Source{RuntimeID: scope.RuntimeID},
		}
		if _, err := r.mem.Add(ctx, scope, entry); err != nil {
			return saved, time.Since(t0), fmt.Errorf("add_raw turn %d (%s): %w", i, t.EvidenceID, err)
		}
		saved++
	}
	return saved, time.Since(t0), nil
}

// Recall implements runners.Runner.
func (r *Runner) Recall(ctx context.Context, scope recall.Scope, query string, topK int) ([]recall.Hit, time.Duration, error) {
	t0 := time.Now()
	hits, err := r.mem.Recall(ctx, scope, recall.Request{Query: query, TopK: topK})
	return hits, time.Since(t0), err
}

// Close implements runners.Runner.
func (r *Runner) Close() error {
	if r.mem == nil {
		return errors.New("flowcraft runner: closed twice")
	}
	err := r.mem.Close()
	r.mem = nil
	return err
}
