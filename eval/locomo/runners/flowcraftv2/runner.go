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
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// Options configures the v2 bootstrap runner.
type Options struct {
	Name string
	LLM  llm.LLM
	// RetrievalIndex is required and must be selected by the caller.
	RetrievalIndex retrieval.Index
	// Embedder, when non-nil, enables hybrid (BM25 + cosine) retrieval
	// on the recall memory: the retrieval projection embeds every
	// indexed fact and the retrieval source embeds each query.
	Embedder embedding.Embedder
	// RerankerLLM, when non-nil, installs an LLM-backed Reranker on
	// the recall memory. Reranking fires post-fusion and pre-cap so
	// the model sees the full overfetched pool (typically 2× topK).
	// nil keeps the default fusion-only ranking.
	RerankerLLM llm.LLM
	// ObservationOnlyIngest saves source turns as Observation ledger rows without
	// running assertion extraction. This is an eval-only ablation for measuring
	// raw evidence fallback without depending on an LLM failure.
	ObservationOnlyIngest bool

	IncludeAssistant bool
	// OnFactsSaved is invoked after each successful Save with the scope and
	// fact ids persisted. nil disables.
	OnFactsSaved func(scope runners.Scope, factIDs []string)
	// OnFactsSavedDetailed is invoked after each successful Save with
	// the SaveRequest, materialized facts persisted by that Save, and,
	// when available, per-save diagnostics. nil disables.
	OnFactsSavedDetailed func(scope runners.Scope, req recall.SaveRequest, facts []recall.TemporalFact, diag *diagnostics.SaveDiagnostics)
	// OnSaveDiagnostics, when non-nil, switches the runner to the SaveExplain
	// path and reports per-stage SaveDiagnostics for every successful Save.
	OnSaveDiagnostics func(scope runners.Scope, diag diagnostics.SaveDiagnostics)
	// OnRecallDiagnostics, when non-nil, switches the runner to the
	// RecallExplain path and reports per-stage RecallDiagnostics for every
	// Recall call.
	OnRecallDiagnostics func(scope runners.Scope, diag diagnostics.RecallDiagnostics)
}

// Runner implements runners.Runner against sdk/recall v2.
type Runner struct {
	name                  string
	mem                   recall.Memory
	saveExplainer         recall.SaveExplainer
	saveDebug             recall.SaveDebugExplainer
	sideEffects           recall.SideEffectProcessor
	recallExplain         recall.RecallExplainer
	hasLLM                bool
	includeAs             bool
	onSaved               func(scope runners.Scope, factIDs []string)
	onFacts               func(scope runners.Scope, req recall.SaveRequest, facts []recall.TemporalFact, diag *diagnostics.SaveDiagnostics)
	onSaveDiag            func(runners.Scope, diagnostics.SaveDiagnostics)
	onRecallDiag          func(runners.Scope, diagnostics.RecallDiagnostics)
	observationOnlyIngest bool
}

const (
	writeAnchorTopK          = 8
	writeAnchorQueryMaxChars = 2000
)

// New constructs a flowcraft-recall-v2 bootstrap runner.
func New(opts Options) (runners.Runner, error) {
	if opts.Name == "" {
		opts.Name = "flowcraft-recall-v2"
	}
	if opts.RetrievalIndex == nil {
		return nil, ErrRetrievalIndexRequired
	}
	memOpts := []recall.Option{
		recall.WithRetrievalIndex(opts.RetrievalIndex),
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
		name:                  opts.Name,
		mem:                   mem,
		hasLLM:                opts.LLM != nil,
		includeAs:             opts.IncludeAssistant,
		onSaved:               opts.OnFactsSaved,
		onFacts:               opts.OnFactsSavedDetailed,
		onSaveDiag:            opts.OnSaveDiagnostics,
		onRecallDiag:          opts.OnRecallDiagnostics,
		observationOnlyIngest: opts.ObservationOnlyIngest,
	}
	if opts.OnSaveDiagnostics != nil || opts.OnFactsSavedDetailed != nil {
		if explainer, ok := mem.(recall.SaveDebugExplainer); ok {
			r.saveDebug = explainer
		}
		if explainer, ok := mem.(recall.SaveExplainer); ok {
			r.saveExplainer = explainer
		}
	}
	if opts.OnRecallDiagnostics != nil {
		if explainer, ok := mem.(recall.RecallExplainer); ok {
			r.recallExplain = explainer
		}
	}
	if proc, ok := recall.NewSideEffectProcessor(mem); ok {
		r.sideEffects = proc
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
	return r.SaveSourceTurnsWithContext(ctx, scope, turns, nil)
}

// SaveSourceTurnsWithContext implements runners.ContextualSourceTurnSaver. It
// mirrors online Save usage: current turns are extractable source_turns, prior
// turns from the same session are recent_context, and recalled memories are
// existing_memory_anchors. Context sections are marked extract=false by the
// recall extractor prompt.
func (r *Runner) SaveSourceTurnsWithContext(ctx context.Context, scope runners.Scope, turns []runners.RawTurn, recentTurns []runners.RawTurn) (int, time.Duration, error) {
	if !r.hasLLM && !r.observationOnlyIngest {
		return 0, 0, ErrExtractorNotSupported
	}
	ctxs, observedAt := buildTurnContexts(turns, r.includeAs)
	if len(ctxs) == 0 {
		return 0, 0, nil
	}
	req := recall.SaveRequest{Turns: ctxs, ObservedAt: observedAt}
	req.RecentMessages = recentMessagesFromRawTurns(recentTurns, r.includeAs)
	anchors, err := r.writeAnchors(ctx, scope, ctxs, req.RecentMessages)
	if err != nil {
		return 0, 0, err
	}
	req.ExistingFactsAnchor = anchors
	return r.runSave(ctx, scope, req)
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
	if r.saveDebug != nil {
		res, trace, err = r.saveDebug.SaveExplainDebug(ctx, toRecallScope(scope), req)
	} else if r.saveExplainer != nil {
		res, trace, err = r.saveExplainer.SaveExplain(ctx, toRecallScope(scope), req)
	} else {
		res, err = r.mem.Save(ctx, toRecallScope(scope), req)
	}
	elapsed := time.Since(t0)
	if err != nil {
		return 0, elapsed, err
	}
	if err := r.drainSideEffects(ctx, scope); err != nil {
		return 0, elapsed, err
	}
	var saveDiag *diagnostics.SaveDiagnostics
	if len(trace.Stages) > 0 {
		d := diagnostics.DiagnoseSave(req, trace)
		saveDiag = &d
	}
	if r.onSaved != nil && len(res.FactIDs) > 0 {
		r.onSaved(scope, res.FactIDs)
	}
	if r.onFacts != nil {
		var facts []recall.TemporalFact
		if len(res.FactIDs) > 0 {
			facts, err = r.savedFacts(ctx, toRecallScope(scope), res.FactIDs)
			if err != nil {
				return 0, elapsed, err
			}
		}
		r.onFacts(scope, req, facts, saveDiag)
	}
	if r.onSaveDiag != nil && saveDiag != nil {
		r.onSaveDiag(scope, *saveDiag)
	}
	return len(res.FactIDs), elapsed, nil
}

func (r *Runner) savedFacts(ctx context.Context, scope recall.Scope, ids []string) ([]recall.TemporalFact, error) {
	out := make([]recall.TemporalFact, 0, len(ids))
	for _, id := range ids {
		versions, err := r.mem.History(ctx, scope, id)
		if err != nil {
			return nil, fmt.Errorf("flowcraftv2: load saved fact %s: %w", id, err)
		}
		if len(versions) == 0 {
			return nil, fmt.Errorf("flowcraftv2: load saved fact %s: not found", id)
		}
		out = append(out, versions[len(versions)-1].Fact)
	}
	return out, nil
}

func (r *Runner) writeAnchors(ctx context.Context, scope runners.Scope, turns []recall.TurnContext, recent []recall.Message) ([]recall.TemporalFact, error) {
	query := writeAnchorQuery(turns, recent)
	if query == "" {
		return nil, nil
	}
	hits, err := r.mem.Recall(ctx, toRecallScope(scope), recall.Query{Text: query, Limit: writeAnchorTopK})
	if err != nil {
		return nil, fmt.Errorf("flowcraftv2: recall write anchors: %w", err)
	}
	anchors := make([]recall.TemporalFact, 0, len(hits))
	seen := map[string]struct{}{}
	for _, hit := range hits {
		if hit.Fact.ID == "" || strings.TrimSpace(hit.Fact.Content) == "" {
			continue
		}
		key := hit.Fact.ID
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(hit.Fact.Content))
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		anchors = append(anchors, hit.Fact.Clone())
		if len(anchors) >= writeAnchorTopK {
			break
		}
	}
	return anchors, nil
}

func writeAnchorQuery(turns []recall.TurnContext, recent []recall.Message) string {
	var parts []string
	for _, msg := range recent {
		if text := strings.TrimSpace(msg.Text); text != "" {
			parts = append(parts, text)
		}
	}
	for _, turn := range turns {
		if text := strings.TrimSpace(turn.Text); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	query := strings.Join(parts, "\n")
	if len(query) <= writeAnchorQueryMaxChars {
		return query
	}
	return query[len(query)-writeAnchorQueryMaxChars:]
}

func recentMessagesFromRawTurns(turns []runners.RawTurn, includeAssistant bool) []recall.Message {
	ctxs, _ := buildTurnContexts(turns, includeAssistant)
	out := make([]recall.Message, 0, len(ctxs))
	for _, turn := range ctxs {
		if strings.TrimSpace(turn.Text) == "" {
			continue
		}
		out = append(out, recall.Message{
			Role:    turn.Role,
			Speaker: turn.Speaker,
			Text:    turn.Text,
			Time:    turn.Time,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Time.IsZero() || out[j].Time.IsZero() {
			return false
		}
		return out[i].Time.Before(out[j].Time)
	})
	return out
}

func (r *Runner) drainSideEffects(ctx context.Context, scope runners.Scope) error {
	if r.sideEffects == nil {
		return nil
	}
	recallScope := toRecallScope(scope)
	for i := 0; i < 64; i++ {
		out, err := r.sideEffects.ProcessSideEffects(ctx, recall.SideEffectProcessOptions{
			Scope: recallScope,
			Limit: 128,
		})
		if err != nil {
			return err
		}
		if out.Failed > 0 {
			return fmt.Errorf("flowcraftv2: side-effect processing failed: %+v", out)
		}
		if out.Claimed == 0 {
			return nil
		}
	}
	return fmt.Errorf("flowcraftv2: side-effect processor did not quiesce for scope %+v", scope)
}

// Recall implements runners.Runner.
func (r *Runner) Recall(ctx context.Context, scope runners.Scope, query string, topK int) ([]runners.RecallArtifact, time.Duration, error) {
	hits, _, elapsed, err := r.recall(ctx, scope, query, topK, r.recallExplain != nil)
	return hits, elapsed, err
}

// RecallWithStageAudit implements runners.RecallStageAuditor.
func (r *Runner) RecallWithStageAudit(ctx context.Context, scope runners.Scope, query string, topK int) ([]runners.RecallArtifact, runners.RecallStageAudit, time.Duration, error) {
	hits, audit, elapsed, err := r.recall(ctx, scope, query, topK, true)
	return hits, audit, elapsed, err
}

func (r *Runner) recall(ctx context.Context, scope runners.Scope, query string, topK int, explain bool) ([]runners.RecallArtifact, runners.RecallStageAudit, time.Duration, error) {
	hits, audit, elapsed, err := r.recallRaw(ctx, scope, query, topK, explain)
	return fromRecallArtifacts(hits), audit, elapsed, err
}

// RecallAnswerContext implements runners.AnswerContextRecaller.
func (r *Runner) RecallAnswerContext(ctx context.Context, scope runners.Scope, question runners.AnswerQuestion, topK int) ([]runners.RecallArtifact, runners.AnswerContext, time.Duration, error) {
	hits, _, elapsed, err := r.recallRaw(ctx, scope, question.Query, topK, r.recallExplain != nil)
	if err != nil {
		return nil, runners.AnswerContext{}, elapsed, err
	}
	return fromRecallArtifacts(hits), structuredAnswerContext(question, hits), elapsed, nil
}

// RecallAnswerContextWithStageAudit implements runners.AnswerContextStageAuditor.
func (r *Runner) RecallAnswerContextWithStageAudit(ctx context.Context, scope runners.Scope, question runners.AnswerQuestion, topK int) ([]runners.RecallArtifact, runners.AnswerContext, runners.RecallStageAudit, time.Duration, error) {
	hits, audit, elapsed, err := r.recallRaw(ctx, scope, question.Query, topK, true)
	if err != nil {
		return nil, runners.AnswerContext{}, audit, elapsed, err
	}
	return fromRecallArtifacts(hits), structuredAnswerContext(question, hits), audit, elapsed, nil
}

func (r *Runner) recallRaw(ctx context.Context, scope runners.Scope, query string, topK int, explain bool) ([]recall.Hit, runners.RecallStageAudit, time.Duration, error) {
	t0 := time.Now()
	q := recall.Query{Text: query, Limit: topK}
	var (
		hits  []recall.Hit
		trace recall.RecallTrace
		err   error
	)
	if explain {
		explainer := r.recallExplain
		if explainer == nil {
			explainer, _ = r.mem.(recall.RecallExplainer)
		}
		if explainer != nil {
			hits, trace, err = explainer.RecallExplain(ctx, toRecallScope(scope), q)
		} else {
			hits, err = r.mem.Recall(ctx, toRecallScope(scope), q)
		}
	} else {
		hits, err = r.mem.Recall(ctx, toRecallScope(scope), q)
	}
	elapsed := time.Since(t0)
	if r.onRecallDiag != nil && len(trace.Stages) > 0 {
		r.onRecallDiag(scope, diagnostics.DiagnoseRecall(trace, hits))
	}
	return hits, fromRecallStageAudit(diagnostics.AuditRecallStages(trace)), elapsed, err
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
var ErrExtractorNotSupported = fmt.Errorf("flowcraft-recall-v2 extractor ingest requires an LLM")

// ErrRetrievalIndexRequired is returned when callers construct the v2 runner
// without explicitly selecting a retrieval backend.
var ErrRetrievalIndexRequired = fmt.Errorf("flowcraft-recall-v2 requires a retrieval index")

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
	"2006/01/02 (Mon) 15:04",
	"2006/01/02 (Monday) 15:04",
	"2006/01/02 15:04",
	"2006/01/02 (Mon)",
	"2006/01/02 (Monday)",
	"2006/01/02",
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
