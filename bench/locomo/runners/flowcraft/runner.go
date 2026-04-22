// Package flowcraft is the default bench Runner: in-memory retrieval Index,
// pipeline.LTM, and the AdditiveExtractor — i.e. exactly what an out-of-the-box
// `ltm.New(Config{Index: memidx.New(), LLM: …})` produces.
package flowcraft

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/bench/locomo/runners"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory/ltm"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

// Options configures the Flowcraft default runner.
//
// All quality-impacting knobs (rerank / soft-merge / context recall / score
// threshold) are SDK-level toggles forwarded straight into ltm.Config and
// pipeline.LTM — bench has no business owning that logic.
type Options struct {
	Name     string
	LLM      llm.LLM            // required iff Save uses the LLM extractor path
	Embedder embedding.Embedder // optional; enables vector lane

	MaxFactsPerCall  int
	IncludeAssistant bool
	ExtractPrompt    string

	// SaveWithContext forwards to ltm.Config.SaveWithContext. When true, the
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
}

// Runner is the default bench Runner.
type Runner struct {
	name string
	mem  ltm.Memory
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
	cfg := ltm.Config{
		Index:            memidx.New(),
		LLM:              opts.LLM,
		Embedder:         opts.Embedder,
		RequireUserID:    true,
		MaxFactsPerCall:  maxFacts,
		IncludeAssistant: opts.IncludeAssistant,
		SaveWithContext:  opts.SaveWithContext,
		MD5Dedup:         true,
	}
	if opts.SoftMerge != nil {
		cfg.SoftMerge = *opts.SoftMerge
	} else {
		cfg.SoftMerge = true
	}
	if opts.ExtractPrompt != "" || opts.LLM != nil {
		cfg.Extractor = &ltm.AdditiveExtractor{
			LLM:              opts.LLM,
			IncludeAssistant: opts.IncludeAssistant,
			MaxFacts:         maxFacts,
			PromptTemplate:   opts.ExtractPrompt,
		}
	}

	pipeOpts := []pipeline.LTMOption{}
	if opts.ScoreThreshold > 0 {
		pipeOpts = append(pipeOpts, pipeline.WithScoreThreshold(opts.ScoreThreshold))
	}
	if opts.RerankerLLM != nil {
		pipeOpts = append(pipeOpts, pipeline.WithReranker(&pipeline.LLMReranker{LLM: opts.RerankerLLM}))
	}
	if len(pipeOpts) > 0 {
		cfg.Pipeline = pipeline.LTM(opts.Embedder, pipeOpts...)
	}

	mem, err := ltm.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Runner{name: opts.Name, mem: mem}, nil
}

// Name implements runners.Runner.
func (r *Runner) Name() string { return r.name }

// Save implements runners.Runner.
func (r *Runner) Save(ctx context.Context, scope ltm.MemoryScope, msgs []llm.Message) (int, time.Duration, error) {
	t0 := time.Now()
	res, err := r.mem.Save(ctx, scope, msgs)
	if err != nil {
		return 0, time.Since(t0), err
	}
	return len(res.EntryIDs), time.Since(t0), nil
}

// SaveRaw is a fallback path for runs without an LLM extractor; one MemoryEntry
// is created per non-empty user/assistant turn. Each entry's ID is
// auto-generated, so recall.k_hit cannot be evaluated through this path —
// callers that need evidence scoring should use SaveRawTurns instead.
func (r *Runner) SaveRaw(ctx context.Context, scope ltm.MemoryScope, msgs []llm.Message) (int, time.Duration, error) {
	t0 := time.Now()
	saved := 0
	for i, m := range msgs {
		txt := m.Content()
		if txt == "" {
			continue
		}
		entry := ltm.MemoryEntry{
			Content:    txt,
			Categories: []string{"raw"},
			Source:     ltm.MemorySource{RuntimeID: scope.RuntimeID},
		}
		if _, err := r.mem.AddRaw(ctx, scope, entry); err != nil {
			return saved, time.Since(t0), fmt.Errorf("add_raw turn %d: %w", i, err)
		}
		saved++
	}
	return saved, time.Since(t0), nil
}

// SaveRawTurns implements runners.RawIngestSaver: it preserves each turn's
// EvidenceID as the MemoryEntry primary key so recall.k_hit becomes
// meaningful. Empty IDs fall back to the auto-generated ULID.
func (r *Runner) SaveRawTurns(ctx context.Context, scope ltm.MemoryScope, turns []runners.RawTurn) (int, time.Duration, error) {
	t0 := time.Now()
	saved := 0
	for i, t := range turns {
		if t.Content == "" {
			continue
		}
		entry := ltm.MemoryEntry{
			ID:         t.EvidenceID,
			Content:    t.Content,
			Categories: []string{"raw"},
			Source:     ltm.MemorySource{RuntimeID: scope.RuntimeID},
		}
		if _, err := r.mem.AddRaw(ctx, scope, entry); err != nil {
			return saved, time.Since(t0), fmt.Errorf("add_raw turn %d (%s): %w", i, t.EvidenceID, err)
		}
		saved++
	}
	return saved, time.Since(t0), nil
}

// Recall implements runners.Runner.
func (r *Runner) Recall(ctx context.Context, scope ltm.MemoryScope, query string, topK int) ([]ltm.RecallHit, time.Duration, error) {
	t0 := time.Now()
	hits, err := r.mem.Recall(ctx, scope, ltm.RecallRequest{Query: query, TopK: topK})
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
