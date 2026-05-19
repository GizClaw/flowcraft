// Package flowcraftv2 is the LoCoMo bench runner for sdk/recall v2.
//
// The current implementation is a bootstrap baseline: each turn is stored
// as a FactNote with evidence refs preserved for recall.k_hit. It validates
// the v2 read path without depending on LLM extraction quality.
package flowcraftv2

import (
	"context"
	"encoding/json"
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

// SaveSourceTurns implements runners.SourceTurnSaver. It renders source-turn
// metadata into SaveRequest.Text so the v2 LLM extractor can cite LoCoMo
// EvidenceID values in source_message_ids / evidence_refs.
func (r *Runner) SaveSourceTurns(ctx context.Context, scope runners.Scope, turns []runners.RawTurn) (int, time.Duration, error) {
	if !r.hasLLM {
		return 0, 0, ErrExtractorNotSupported
	}
	text, n := renderTurns(turns, r.includeAs)
	if n == 0 {
		return 0, 0, nil
	}
	return r.runSave(ctx, scope, recall.SaveRequest{Text: text})
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

const renderTurnsHeader = `FLOWCRAFT_RECALL_TURNS_V1
Each following line is one source turn as JSON. When extracting a fact, cite supporting turn_id values in source_message_ids and evidence_refs.id.
`

type renderedTurn struct {
	TurnID     string `json:"turn_id"`
	EvidenceID string `json:"evidence_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Role       string `json:"role"`
	Text       string `json:"text"`
}

func renderTurns(turns []runners.RawTurn, includeAssistant bool) (string, int) {
	var b strings.Builder
	b.WriteString(renderTurnsHeader)
	n := 0
	for i, t := range turns {
		role := strings.TrimSpace(t.Role)
		text := strings.TrimSpace(t.Content)
		if text == "" {
			continue
		}
		if !includeAssistant && model.Role(role) == model.RoleAssistant {
			continue
		}
		line, err := json.Marshal(renderedTurn{
			TurnID:     turnID(t, i),
			EvidenceID: strings.TrimSpace(t.EvidenceID),
			SessionID:  strings.TrimSpace(t.SessionID),
			Role:       role,
			Text:       text,
		})
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
		n++
	}
	if n == 0 {
		return "", 0
	}
	return b.String(), n
}

func turnID(t runners.RawTurn, idx int) string {
	if id := strings.TrimSpace(t.EvidenceID); id != "" {
		return id
	}
	return fmt.Sprintf("turn_%04d", idx+1)
}
