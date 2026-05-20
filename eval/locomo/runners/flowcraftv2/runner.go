// Package flowcraftv2 is the LoCoMo bench runner for sdk/recall v2.
//
// The current implementation is a bootstrap baseline: each turn is stored
// as a FactNote with evidence refs preserved for recall.k_hit. It validates
// the v2 read path without depending on LLM extraction quality.
package flowcraftv2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// Options configures the v2 bootstrap runner.
type Options struct {
	Name string
	LLM  llm.LLM
	// Embedder, when non-nil, enables hybrid (BM25 + cosine) retrieval
	// on the recall memory: the retrieval projection embeds every
	// indexed fact and the retrieval source embeds each query.
	Embedder embedding.Embedder
	// RerankerLLM, when non-nil, installs an LLM-backed Reranker on
	// the recall memory. Reranking fires post-fusion and pre-cap so
	// the model sees the full overfetched pool (typically 2× topK).
	// nil keeps the default fusion-only ranking.
	RerankerLLM llm.LLM

	IncludeAssistant bool
	// OnFactsSaved is invoked after each successful Save with the scope and
	// fact ids persisted. nil disables.
	OnFactsSaved func(scope runners.Scope, factIDs []string)
	// OnSaveDiagnostics, when non-nil, switches the runner to the SaveExplain
	// path and reports per-stage SaveDiagnostics for every successful Save.
	OnSaveDiagnostics func(scope runners.Scope, diag recall.SaveDiagnostics)
	// OnRecallDiagnostics, when non-nil, switches the runner to the
	// RecallExplain path and reports per-stage RecallDiagnostics for every
	// Recall call.
	OnRecallDiagnostics func(scope runners.Scope, diag recall.RecallDiagnostics)
}

// Runner implements runners.Runner against sdk/recall v2.
type Runner struct {
	name          string
	mem           recall.Memory
	saveExplainer recall.SaveExplainer
	recallExplain recall.RecallExplainer
	hasLLM        bool
	includeAs     bool
	onSaved       func(scope runners.Scope, factIDs []string)
	onSaveDiag    func(runners.Scope, recall.SaveDiagnostics)
	onRecallDiag  func(runners.Scope, recall.RecallDiagnostics)
}

// New constructs a flowcraft-v2 bootstrap runner.
func New(opts Options) (runners.Runner, error) {
	if opts.Name == "" {
		opts.Name = "flowcraft-v2"
	}
	memOpts := []recall.Option{
		recall.WithRetrievalIndex(retrievalmem.New()),
		// Enable the EntityGraph projection + graph source. Multi-hop
		// LoCoMo questions ("how is A connected to B?") rely on
		// n-hop entity traversal that the other four structured
		// sources cannot do on their own.
		recall.WithGraphEnabled(true),
	}
	if opts.LLM != nil {
		memOpts = append(memOpts, recall.WithLLMExtractor(opts.LLM))
	}
	if opts.Embedder != nil {
		memOpts = append(memOpts, recall.WithEmbedder(opts.Embedder))
	}
	if opts.RerankerLLM != nil {
		memOpts = append(memOpts, recall.WithReranker(recall.NewLLMReranker(opts.RerankerLLM)))
	}
	mem, err := recall.New(memOpts...)
	if err != nil {
		return nil, err
	}
	r := &Runner{
		name:         opts.Name,
		mem:          mem,
		hasLLM:       opts.LLM != nil,
		includeAs:    opts.IncludeAssistant,
		onSaved:      opts.OnFactsSaved,
		onSaveDiag:   opts.OnSaveDiagnostics,
		onRecallDiag: opts.OnRecallDiagnostics,
	}
	if opts.OnSaveDiagnostics != nil {
		if explainer, ok := mem.(recall.SaveExplainer); ok {
			r.saveExplainer = explainer
		}
	}
	if opts.OnRecallDiagnostics != nil {
		if explainer, ok := mem.(recall.RecallExplainer); ok {
			r.recallExplain = explainer
		}
	}
	return r, nil
}

// Name implements runners.Runner.
func (r *Runner) Name() string { return r.name }

// Save implements runners.Runner. Bootstrap mode writes one FactNote per
// non-empty message without evidence refs (k_hit is not meaningful on this path).
func (r *Runner) Save(ctx context.Context, scope runners.Scope, msgs []llm.Message) (int, time.Duration, error) {
	facts := make([]recall.TemporalFact, 0, len(msgs))
	for _, m := range msgs {
		if !r.includeAs && m.Role == model.RoleAssistant {
			continue
		}
		txt := m.Content()
		if txt == "" {
			continue
		}
		facts = append(facts, recall.TemporalFact{
			Kind:    recall.FactNote,
			Content: txt,
		})
	}
	return r.saveFacts(ctx, scope, facts)
}

// SaveRaw implements the optional raw-ingest path used by eval locomo ingest.
func (r *Runner) SaveRaw(ctx context.Context, scope runners.Scope, msgs []llm.Message) (int, time.Duration, error) {
	return r.Save(ctx, scope, msgs)
}

// SaveRawTurns implements runners.RawIngestSaver. Each turn's EvidenceID is
// preserved on the fact and in EvidenceRefs so recall.k_hit can match dia_id.
func (r *Runner) SaveRawTurns(ctx context.Context, scope runners.Scope, turns []runners.RawTurn) (int, time.Duration, error) {
	facts := make([]recall.TemporalFact, 0, len(turns))
	for _, t := range turns {
		if t.Content == "" {
			continue
		}
		f := recall.TemporalFact{
			Kind:    recall.FactNote,
			Content: t.Content,
		}
		if t.EvidenceID != "" {
			f.ID = t.EvidenceID
			f.EvidenceRefs = []recall.EvidenceRef{{
				ID:   t.EvidenceID,
				Role: t.Role,
				Text: t.Content,
			}}
		}
		facts = append(facts, f)
	}
	return r.saveFacts(ctx, scope, facts)
}

// SaveSourceTurns implements runners.SourceTurnSaver. It converts each
// RawTurn into a typed recall.TurnContext (parsing the optional
// "[<time>] <speaker>:" prefix LoCoMo bakes into raw content) and
// passes the typed channel through SaveRequest.Turns. The SDK
// extractor handles JSONL rendering; this adapter only owns the
// RawTurn -> TurnContext shape conversion.
func (r *Runner) SaveSourceTurns(ctx context.Context, scope runners.Scope, turns []runners.RawTurn) (int, time.Duration, error) {
	if !r.hasLLM {
		return 0, 0, ErrExtractorNotSupported
	}
	ctxs, observedAt := buildTurnContexts(turns, r.includeAs)
	if len(ctxs) == 0 {
		return 0, 0, nil
	}
	return r.runSave(ctx, scope, recall.SaveRequest{Turns: ctxs, ObservedAt: observedAt})
}

func (r *Runner) saveFacts(ctx context.Context, scope runners.Scope, facts []recall.TemporalFact) (int, time.Duration, error) {
	if len(facts) == 0 {
		return 0, 0, nil
	}
	return r.runSave(ctx, scope, recall.SaveRequest{Facts: facts})
}

func (r *Runner) runSave(ctx context.Context, scope runners.Scope, req recall.SaveRequest) (int, time.Duration, error) {
	t0 := time.Now()
	var (
		res   recall.SaveResult
		trace recall.SaveTrace
		err   error
	)
	if r.saveExplainer != nil {
		res, trace, err = r.saveExplainer.SaveExplain(ctx, toRecallScope(scope), req)
	} else {
		res, err = r.mem.Save(ctx, toRecallScope(scope), req)
	}
	elapsed := time.Since(t0)
	if err != nil {
		return 0, elapsed, err
	}
	if r.onSaved != nil && len(res.FactIDs) > 0 {
		r.onSaved(scope, res.FactIDs)
	}
	if r.onSaveDiag != nil && r.saveExplainer != nil {
		r.onSaveDiag(scope, recall.DiagnoseSave(req, trace))
	}
	return len(res.FactIDs), elapsed, nil
}

// Recall implements runners.Runner.
func (r *Runner) Recall(ctx context.Context, scope runners.Scope, query string, topK int) ([]runners.Hit, time.Duration, error) {
	t0 := time.Now()
	q := recall.Query{Text: query, Limit: topK}
	var (
		hits  []recall.Hit
		trace recall.RecallTrace
		err   error
	)
	if r.recallExplain != nil {
		hits, trace, err = r.recallExplain.RecallExplain(ctx, toRecallScope(scope), q)
	} else {
		hits, err = r.mem.Recall(ctx, toRecallScope(scope), q)
	}
	elapsed := time.Since(t0)
	if r.onRecallDiag != nil && r.recallExplain != nil {
		r.onRecallDiag(scope, recall.DiagnoseRecall(trace, hits))
	}
	return fromRecallHits(hits), elapsed, err
}

// Close implements runners.Runner.
func (r *Runner) Close() error {
	if r.mem == nil {
		return errors.New("flowcraftv2 runner: closed twice")
	}
	err := r.mem.Close()
	r.mem = nil
	return err
}

// Baseline describes the runner's internal capability profile for reports.
const Baseline = "bootstrap-raw"

// ErrExtractorNotSupported is returned when callers request LLM extraction
// without wiring an LLM into the v2 runner.
var ErrExtractorNotSupported = fmt.Errorf("flowcraft-v2 extractor ingest requires an LLM")

// buildTurnContexts maps the LoCoMo-shaped RawTurns into typed
// recall.TurnContexts. The LoCoMo dataset stuffs absolute
// timestamps and canonical speaker names into the turn content as a
// "[<time>] <speaker>:" prefix; this function strips that prefix
// and lifts the components into typed Time / Speaker fields so the
// SDK extractor never has to grep prose for them.
//
// ObservedAt is the earliest typed Time across the batch; the
// compiler uses it as the wall-clock anchor for relative-time
// resolution. Zero when no turn has a typed timestamp (synthetic
// data, raw chat dumps) — the compiler then falls back to its
// real-time clock.
//
// Assistant turns are filtered when includeAssistant=false; empty /
// whitespace-only turns are always dropped.
func buildTurnContexts(turns []runners.RawTurn, includeAssistant bool) ([]recall.TurnContext, time.Time) {
	out := make([]recall.TurnContext, 0, len(turns))
	var observedAt time.Time
	for i, t := range turns {
		role := strings.TrimSpace(t.Role)
		raw := strings.TrimSpace(t.Content)
		if raw == "" {
			continue
		}
		if !includeAssistant && model.Role(role) == model.RoleAssistant {
			continue
		}
		ts, speaker, body := splitTurnPrefix(raw)
		typedTime := parseLocomoTimestamp(ts)
		if !typedTime.IsZero() && (observedAt.IsZero() || typedTime.Before(observedAt)) {
			observedAt = typedTime
		}
		out = append(out, recall.TurnContext{
			ID:         turnID(t, i),
			EvidenceID: strings.TrimSpace(t.EvidenceID),
			SessionID:  strings.TrimSpace(t.SessionID),
			Role:       role,
			Speaker:    speaker,
			Time:       typedTime,
			Text:       body,
		})
	}
	return out, observedAt
}

// splitTurnPrefix pulls the optional "[<time>] <speaker>: <body>"
// prefix the LoCoMo convert step bakes into each turn's content.
// Both the bracketed time and the trailing "speaker: " are stripped
// from body so the text the LLM reads is clean prose; the same
// information is reinjected via the typed Time / Speaker fields on
// TurnContext. Returns ts="" and speaker="" when the prefix is
// absent so the adapter degrades cleanly for raw chat dumps.
func splitTurnPrefix(raw string) (ts, speaker, body string) {
	body = raw
	if strings.HasPrefix(body, "[") {
		if end := strings.Index(body, "]"); end > 0 {
			ts = strings.TrimSpace(body[1:end])
			body = strings.TrimSpace(body[end+1:])
		}
	}
	if colon := strings.Index(body, ":"); colon > 0 {
		head := strings.TrimSpace(body[:colon])
		if speakerLooksClean(head) {
			speaker = head
			body = strings.TrimSpace(body[colon+1:])
		}
	}
	return ts, speaker, body
}

// locomoTimeLayouts is the small set of date / datetime shapes the
// LoCoMo convert step bakes into the "[…]" prefix. It is closed on
// purpose: anything outside this set is left as a string in the
// preserved Time field and the compiler degrades to relative-time
// grounding.
var locomoTimeLayouts = []string{
	"3:04 pm on 2 January, 2006",
	"3:04 pm on 2 Jan, 2006",
	"15:04 on 2 January, 2006",
	"15:04 on 2 Jan, 2006",
	"3:04 pm on 2 January 2006",
	"2 January, 2006",
	"2 January 2006",
	"2 Jan 2006",
	"January 2, 2006",
	"Jan 2, 2006",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	time.RFC3339,
}

func parseLocomoTimestamp(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range locomoTimeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func speakerLooksClean(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			return false
		}
	}
	return true
}

func turnID(t runners.RawTurn, idx int) string {
	if id := strings.TrimSpace(t.EvidenceID); id != "" {
		return id
	}
	return fmt.Sprintf("turn_%04d", idx+1)
}
