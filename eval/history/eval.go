package history

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/metrics"
	sdkhistory "github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Strategy enumerates the three loading modes the bench compares.
type Strategy string

const (
	// StrategyNone passes the whole transcript verbatim. Quality upper bound;
	// also the most expensive in tokens.
	StrategyNone Strategy = "none"
	// StrategyBuffer keeps only the most recent BufferMax messages.
	// Cheap baseline; demonstrates what tail-truncation costs in quality.
	StrategyBuffer Strategy = "buffer"
	// StrategyCompacted uses sdk/history.NewCompacted (DAG summarizer).
	// Requires an LLM for summarization.
	StrategyCompacted Strategy = "compacted"
)

// Options controls the bench run.
type Options struct {
	// AnswerLLM synthesizes the answer from the loaded history + question.
	// Required.
	AnswerLLM llm.LLM
	// SummaryLLM is the model the compactor uses to roll up older turns.
	// If nil and Strategies includes StrategyCompacted, that strategy is
	// skipped with a logged warning.
	SummaryLLM llm.LLM
	// Judge defaults to metrics.EMJudge when nil. Plug in metrics.LLMJudge for
	// production reports.
	Judge metrics.Judge

	// AnswerPrompt is a fmt template; %s is replaced by
	// "HISTORY:\n<msgs>\n\nQUESTION: <q>".
	// Default: see DefaultAnswerPrompt.
	AnswerPrompt string

	// BufferMax bounds StrategyBuffer's window. Default: 10 messages.
	BufferMax int

	// CompactTokenBudget caps the assembled context size for StrategyCompacted.
	// Default: 1500 tokens (typical of a "long conversation, short prompt"
	// production setup).
	CompactTokenBudget int

	// Strategies selects which strategies to run. Empty = all three.
	Strategies []Strategy

	// LimitConvs / LimitQs trim the dataset for debug runs.
	LimitConvs int
	LimitQs    int

	// Concurrency caps how many questions are answered in parallel per
	// strategy. <=1 means strictly sequential (legacy behavior). The
	// answer/judge LLM is the dominant cost so this is the only knob that
	// matters in practice.
	Concurrency int

	// ProgressEvery, when > 0, logs a "X/N done" line to the standard
	// logger every ProgressEvery completed questions. Use 0 for silent
	// runs (tests).
	ProgressEvery int

	// Hook, when non-nil, is invoked at every lifecycle checkpoint
	// (start, per-strategy progress, strategy_done, done, error). The
	// CLI wires this to a Feishu notifier so 30+-min runs surface a
	// live status card; tests leave it nil for hermetic determinism.
	Hook EventHook

	// ProgressPct gates intra-strategy progress events: each strategy's
	// QA loop fires a progress event every ProgressPct percent of
	// completed questions. <=0 disables intermediate progress entirely
	// (start/strategy_done/done still fire).
	ProgressPct int
}

// Event is the payload pushed to [Options.Hook] at each lifecycle moment.
//
// Kinds emitted by [Run]:
//   - "start"            — before any strategy runs (Fields: dataset, n_convs, n_qs, strategies)
//   - "strategy_start"   — before a strategy begins ingest+QA (Fields: strategy)
//   - "strategy_progress"— every ProgressPct % of QA done (Fields: strategy, done, total)
//   - "strategy_done"    — after a strategy completes (Fields: strategy, qa_judge, qa_em, qa_f1, prompt_tokens_p95, errors)
//   - "done"             — final summary across strategies (Fields: duration, plus per-strategy qa_judge.<s>)
//   - "error"            — a strategy aborted (Fields: strategy, err)
//
// The shape mirrors notify.Event field-for-field so adapters can copy by
// value rather than re-deriving each map.
type Event struct {
	Kind   string
	Time   time.Time
	Title  string
	Body   string
	Fields map[string]string
}

// EventHook receives lifecycle events. It MUST be non-blocking on the hot
// path: Run invokes it synchronously and a slow notifier would stall the
// next ingest/QA batch.
type EventHook func(ctx context.Context, e Event)

// DefaultAnswerPrompt mirrors the LoCoMo answer prompt's neutral framing so
// quality numbers across the two benches are comparable.
const DefaultAnswerPrompt = `You are an assistant who answers a question using only the prior conversation provided.

%s

Guidelines:
- Ground the answer in the conversation; do not invent facts.
- Paraphrase rather than quoting verbatim.
- If the conversation does not contain enough information, say so.

Answer:`

// PerStrategyReport summarizes one strategy's run.
type PerStrategyReport struct {
	Strategy Strategy `json:"strategy"`
	Skipped  string   `json:"skipped,omitempty"`

	Judge float64 `json:"qa_judge"`
	EM    float64 `json:"qa_em"`
	F1    float64 `json:"qa_f1"`

	PromptTokensP50 int `json:"prompt_tokens_p50"`
	PromptTokensP95 int `json:"prompt_tokens_p95"`
	PromptTokensMax int `json:"prompt_tokens_max"`

	LoadLatencyP50 time.Duration `json:"history_load_p50"`
	LoadLatencyP95 time.Duration `json:"history_load_p95"`

	N      int `json:"n"`
	Errors int `json:"errors"`

	// EvidenceMeasured is the number of questions that carried at least one
	// evidence_id (i.e. the subset on which Truncated is meaningful).
	// Datasets without evidence_ids report 0 for both.
	EvidenceMeasured int `json:"evidence_measured"`
	// Truncated counts evidence-bearing questions for which the loaded prompt
	// no longer contains any of the evidence turns' content. Reading: a
	// non-zero value pinpoints "this strategy compressed away the evidence
	// the question requires". For StrategyNone this should always be 0; if
	// it is not, the dataset's evidence_ids are inconsistent with its
	// transcripts and the rest of the report should not be trusted.
	Truncated int `json:"truncated"`
	// TruncatedRate = Truncated / EvidenceMeasured (NaN serializes as 0).
	TruncatedRate float64 `json:"truncated_rate"`
}

// Report is the top-level JSON document the cmd writes.
type Report struct {
	Dataset    string                          `json:"dataset"`
	StartedAt  time.Time                       `json:"started_at"`
	DurationMS int64                           `json:"duration_ms"`
	Options    map[string]any                  `json:"options"`
	Strategies map[Strategy]*PerStrategyReport `json:"strategies"`
}

// Run replays ds across the configured strategies and returns a Report.
func Run(ctx context.Context, ds *dataset.Dataset, opts Options) (*Report, error) {
	if opts.AnswerLLM == nil {
		return nil, fmt.Errorf("history-compression: Options.AnswerLLM is required")
	}
	if opts.Judge == nil {
		opts.Judge = metrics.EMJudge{}
	}
	if opts.AnswerPrompt == "" {
		opts.AnswerPrompt = DefaultAnswerPrompt
	}
	if opts.BufferMax <= 0 {
		opts.BufferMax = 10
	}
	if opts.CompactTokenBudget <= 0 {
		opts.CompactTokenBudget = 1500
	}
	if len(opts.Strategies) == 0 {
		opts.Strategies = []Strategy{StrategyNone, StrategyBuffer, StrategyCompacted}
	}
	ds = trimDataset(ds, opts.LimitConvs, opts.LimitQs)

	rep := &Report{
		Dataset:    ds.Name,
		StartedAt:  time.Now(),
		Strategies: map[Strategy]*PerStrategyReport{},
		Options: map[string]any{
			"buffer_max":           opts.BufferMax,
			"compact_token_budget": opts.CompactTokenBudget,
		},
	}
	defer func() { rep.DurationMS = time.Since(rep.StartedAt).Milliseconds() }()

	emit := func(e Event) {
		if opts.Hook == nil {
			return
		}
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		opts.Hook(ctx, e)
	}

	stratNames := make([]string, 0, len(opts.Strategies))
	for _, s := range opts.Strategies {
		stratNames = append(stratNames, string(s))
	}
	emit(Event{
		Kind:  "start",
		Title: ds.Name,
		Body:  fmt.Sprintf("history-compression — %d conv / %d qs across %d strategies", len(ds.Conversations), len(ds.Questions), len(opts.Strategies)),
		Fields: map[string]string{
			"dataset":    ds.Name,
			"n_convs":    fmt.Sprintf("%d", len(ds.Conversations)),
			"n_qs":       fmt.Sprintf("%d", len(ds.Questions)),
			"strategies": strings.Join(stratNames, ","),
		},
	})

	for _, s := range opts.Strategies {
		emit(Event{
			Kind:   "strategy_start",
			Title:  string(s),
			Body:   fmt.Sprintf("strategy %s starting", s),
			Fields: map[string]string{"strategy": string(s)},
		})
		r, err := runStrategy(ctx, ds, s, opts, emit)
		if err != nil {
			emit(Event{
				Kind:   "error",
				Title:  string(s),
				Body:   err.Error(),
				Fields: map[string]string{"strategy": string(s), "err": err.Error()},
			})
			return nil, fmt.Errorf("strategy %s: %w", s, err)
		}
		rep.Strategies[s] = r
		emit(Event{
			Kind:  "strategy_done",
			Title: string(s),
			Body:  fmt.Sprintf("judge=%.3f em=%.3f f1=%.3f prompt_p95=%d errors=%d", r.Judge, r.EM, r.F1, r.PromptTokensP95, r.Errors),
			Fields: map[string]string{
				"strategy":          string(s),
				"qa_judge":          fmt.Sprintf("%.3f", r.Judge),
				"qa_em":             fmt.Sprintf("%.3f", r.EM),
				"qa_f1":             fmt.Sprintf("%.3f", r.F1),
				"prompt_tokens_p95": fmt.Sprintf("%d", r.PromptTokensP95),
				"errors":            fmt.Sprintf("%d", r.Errors),
			},
		})
	}

	doneFields := map[string]string{"duration": time.Since(rep.StartedAt).Round(time.Second).String()}
	doneParts := make([]string, 0, len(opts.Strategies))
	for _, s := range opts.Strategies {
		if r := rep.Strategies[s]; r != nil {
			doneFields["judge_"+string(s)] = fmt.Sprintf("%.3f", r.Judge)
			doneParts = append(doneParts, fmt.Sprintf("%s=%.3f", s, r.Judge))
		}
	}
	emit(Event{
		Kind:   "done",
		Title:  ds.Name,
		Body:   strings.Join(doneParts, " | "),
		Fields: doneFields,
	})
	return rep, nil
}

func runStrategy(ctx context.Context, ds *dataset.Dataset, s Strategy, opts Options, emit func(Event)) (*PerStrategyReport, error) {
	r := &PerStrategyReport{Strategy: s}
	if s == StrategyCompacted && opts.SummaryLLM == nil {
		r.Skipped = "summary-llm not configured"
		return r, nil
	}

	hist, ingest, err := buildHistory(s, opts)
	if err != nil {
		return nil, err
	}
	if c, ok := hist.(interface{ Close() }); ok {
		defer c.Close()
	}

	// evidenceFingerprints[convID][evidenceID] = normalized snippet of the
	// turn whose evidence_id we want to recover post-Load. We keep at most
	// evidenceFingerprintLen runes of each turn so the substring check stays
	// robust against compactor rephrasing of trailing words while still
	// being unique inside a conversation.
	evidence := map[string]map[string]string{}
	for _, conv := range ds.Conversations {
		if err := ingest(ctx, conv); err != nil {
			return nil, fmt.Errorf("ingest %s: %w", conv.ID, err)
		}
		evidence[conv.ID] = fingerprintTurns(conv.Turns)
	}

	counter := &sdkhistory.EstimateCounter{}
	type sample struct {
		tokens     int
		loadLat    time.Duration
		judge      float64
		f1         float64
		em         bool
		evMeasured bool
		evTrunc    bool
		err        bool
	}
	results := make([]sample, len(ds.Questions))

	concurrency := opts.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var done int64
	total := len(ds.Questions)

	// progress milestones: integer thresholds derived from ProgressPct.
	// We compute them once (rather than dividing inside the hot loop) so
	// the doneCounter only does a single atomic compare-with-array lookup.
	var milestones []int64
	if total > 0 && opts.ProgressPct > 0 && emit != nil {
		for pct := opts.ProgressPct; pct <= 99; pct += opts.ProgressPct {
			ms := int64(total) * int64(pct) / 100
			if ms < 1 {
				ms = 1
			}
			milestones = append(milestones, ms)
		}
	}
	var nextMilestoneIdx int64 // first unsent milestone

	for i := range ds.Questions {
		i := i
		q := ds.Questions[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			out := sample{}
			t0 := time.Now()
			// Note: hist.Load is safe for concurrent calls per the
			// History contract (per-conversation serialization is
			// internal). Append happens only during ingest, before
			// this loop.
			msgs, err := hist.Load(ctx, q.ConversationID, sdkhistory.Budget{MaxTokens: opts.CompactTokenBudget})
			out.loadLat = time.Since(t0)
			if err != nil {
				out.err = true
				results[i] = out
				return
			}

			loaded := formatHistory(msgs)
			body := loaded + "\n\nQUESTION: " + q.Query
			prompt := fmt.Sprintf(opts.AnswerPrompt, body)
			out.tokens = counter.Count(prompt)

			if len(q.EvidenceIDs) > 0 {
				out.evMeasured = true
				if !evidencePresent(loaded, evidence[q.ConversationID], q.EvidenceIDs) {
					out.evTrunc = true
				}
			}

			resp, _, err := opts.AnswerLLM.Generate(ctx, []llm.Message{
				{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: prompt}}},
			})
			if err != nil {
				out.err = true
				results[i] = out
				return
			}
			pred := resp.Content()
			score, err := opts.Judge.Score(ctx, q.Query, pred, q.GoldAnswers)
			if err != nil {
				out.err = true
				results[i] = out
				return
			}
			out.judge = score
			out.f1 = metrics.F1(pred, q.GoldAnswers)
			out.em = metrics.ExactMatch(pred, q.GoldAnswers)
			results[i] = out

			d := atomic.AddInt64(&done, 1)
			if opts.ProgressEvery > 0 && d%int64(opts.ProgressEvery) == 0 {
				log.Printf("[history-compression] %s %d/%d questions done", s, d, total)
			}
			// Milestone emission: cheap fast-path (one atomic load) for
			// the common case where no milestone has been crossed.
			if len(milestones) > 0 {
				idx := atomic.LoadInt64(&nextMilestoneIdx)
				if idx < int64(len(milestones)) && d >= milestones[idx] {
					// Only the goroutine that wins the CAS sends the
					// event; others observe the bumped idx and skip.
					if atomic.CompareAndSwapInt64(&nextMilestoneIdx, idx, idx+1) {
						pct := int64(opts.ProgressPct) * (idx + 1)
						emit(Event{
							Kind: "strategy_progress",
							Body: fmt.Sprintf("%s %d/%d (~%d%%)", s, d, total, pct),
							Fields: map[string]string{
								"strategy": string(s),
								"done":     fmt.Sprintf("%d", d),
								"total":    fmt.Sprintf("%d", total),
								"pct":      fmt.Sprintf("%d", pct),
							},
						})
					}
				}
			}
		}()
	}
	wg.Wait()

	tokens := make([]int, 0, total)
	loadLats := make([]time.Duration, 0, total)
	var sumJudge, sumF1, sumEM float64
	for _, s := range results {
		if s.err {
			r.Errors++
			continue
		}
		tokens = append(tokens, s.tokens)
		loadLats = append(loadLats, s.loadLat)
		sumJudge += s.judge
		sumF1 += s.f1
		if s.em {
			sumEM++
		}
		if s.evMeasured {
			r.EvidenceMeasured++
			if s.evTrunc {
				r.Truncated++
			}
		}
	}

	n := total - r.Errors
	if n < 1 {
		n = 1
	}
	r.N = n
	r.Judge = sumJudge / float64(n)
	r.F1 = sumF1 / float64(n)
	r.EM = sumEM / float64(n)
	r.PromptTokensP50, r.PromptTokensP95, r.PromptTokensMax = tokenStats(tokens)
	r.LoadLatencyP50, r.LoadLatencyP95 = latencyPercentiles(loadLats)
	if r.EvidenceMeasured > 0 {
		r.TruncatedRate = float64(r.Truncated) / float64(r.EvidenceMeasured)
	}
	return r, nil
}

// evidenceFingerprintLen bounds how much of each evidence turn we use as the
// presence marker in the loaded prompt. ~32 normalized runes is enough to be
// unique inside a single conversation without forcing verbatim survival of
// long trailing sentences (which the compactor is allowed to drop).
const evidenceFingerprintLen = 32

func fingerprintTurns(turns []dataset.Turn) map[string]string {
	out := map[string]string{}
	for _, t := range turns {
		if t.EvidenceID == "" {
			continue
		}
		fp := metrics.Normalize(t.Content)
		if r := []rune(fp); len(r) > evidenceFingerprintLen {
			fp = string(r[:evidenceFingerprintLen])
		}
		if fp == "" {
			continue
		}
		out[t.EvidenceID] = fp
	}
	return out
}

// evidencePresent returns true iff at least one of want's fingerprints is a
// substring of the loaded transcript. Mirrors the "any-hit" semantics
// eval/locomo's evidenceKHit uses, so the two reports stay comparable.
func evidencePresent(loaded string, fps map[string]string, want []string) bool {
	if len(fps) == 0 {
		return false
	}
	norm := metrics.Normalize(loaded)
	for _, id := range want {
		fp, ok := fps[id]
		if !ok {
			continue
		}
		if strings.Contains(norm, fp) {
			return true
		}
	}
	return false
}

// buildHistory returns the History impl for the strategy plus an ingest
// function that pre-loads each conversation. The compactor variant uses an
// in-memory workspace so the bench is hermetic.
func buildHistory(s Strategy, opts Options) (sdkhistory.History, func(context.Context, dataset.Conversation) error, error) {
	switch s {
	case StrategyNone, StrategyBuffer:
		max := opts.BufferMax
		if s == StrategyNone {
			max = 1 << 30 // unbounded
		}
		h := sdkhistory.NewBuffer(sdkhistory.NewInMemoryStore(), sdkhistory.WithBufferMax(max))
		return h, ingestVerbatim(h), nil
	case StrategyCompacted:
		ws := workspace.NewMemWorkspace()
		h := sdkhistory.NewCompacted(sdkhistory.NewInMemoryStore(), opts.SummaryLLM, ws,
			sdkhistory.WithTokenBudget(opts.CompactTokenBudget),
		)
		return h, ingestVerbatim(h), nil
	default:
		return nil, nil, fmt.Errorf("unknown strategy %q", s)
	}
}

func ingestVerbatim(h sdkhistory.History) func(context.Context, dataset.Conversation) error {
	return func(ctx context.Context, conv dataset.Conversation) error {
		msgs := make([]model.Message, 0, len(conv.Turns))
		for _, t := range conv.Turns {
			msgs = append(msgs, model.NewTextMessage(roleOf(t.Role), t.Content))
		}
		return h.Append(ctx, conv.ID, msgs)
	}
}

func roleOf(r string) model.Role {
	switch strings.ToLower(r) {
	case "assistant", "bot":
		return model.RoleAssistant
	case "system":
		return model.RoleSystem
	default:
		return model.RoleUser
	}
}

func formatHistory(msgs []model.Message) string {
	var b strings.Builder
	b.WriteString("HISTORY:\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, textOf(m))
	}
	return b.String()
}

func textOf(m model.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == model.PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func trimDataset(ds *dataset.Dataset, limitConvs, limitQs int) *dataset.Dataset {
	if limitConvs > 0 && len(ds.Conversations) > limitConvs {
		ds.Conversations = ds.Conversations[:limitConvs]
		keep := map[string]struct{}{}
		for _, c := range ds.Conversations {
			keep[c.ID] = struct{}{}
		}
		filtered := ds.Questions[:0]
		for _, q := range ds.Questions {
			if _, ok := keep[q.ConversationID]; ok {
				filtered = append(filtered, q)
			}
		}
		ds.Questions = filtered
	}
	if limitQs > 0 && len(ds.Questions) > limitQs {
		ds.Questions = ds.Questions[:limitQs]
	}
	return ds
}

func tokenStats(samples []int) (p50, p95, max int) {
	if len(samples) == 0 {
		return 0, 0, 0
	}
	sorted := append([]int(nil), samples...)
	sort.Ints(sorted)
	p50 = sorted[len(sorted)/2]
	p95 = sorted[(len(sorted)*95)/100]
	max = sorted[len(sorted)-1]
	return
}

func latencyPercentiles(samples []time.Duration) (p50, p95 time.Duration) {
	if len(samples) == 0 {
		return 0, 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 = sorted[len(sorted)/2]
	p95 = sorted[(len(sorted)*95)/100]
	return
}
