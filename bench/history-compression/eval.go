package historycompression

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/bench/locomo/dataset"
	"github.com/GizClaw/flowcraft/bench/locomo/metrics"
	"github.com/GizClaw/flowcraft/sdk/history"
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
}

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

	N         int `json:"n"`
	Errors    int `json:"errors"`
	Truncated int `json:"truncated"` // count where buffer/compactor likely dropped evidence
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

	for _, s := range opts.Strategies {
		r, err := runStrategy(ctx, ds, s, opts)
		if err != nil {
			return nil, fmt.Errorf("strategy %s: %w", s, err)
		}
		rep.Strategies[s] = r
	}
	return rep, nil
}

func runStrategy(ctx context.Context, ds *dataset.Dataset, s Strategy, opts Options) (*PerStrategyReport, error) {
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

	for _, conv := range ds.Conversations {
		if err := ingest(ctx, conv); err != nil {
			return nil, fmt.Errorf("ingest %s: %w", conv.ID, err)
		}
	}

	counter := &history.EstimateCounter{}
	tokens := make([]int, 0, len(ds.Questions))
	loadLats := make([]time.Duration, 0, len(ds.Questions))
	var sumJudge, sumF1, sumEM float64
	for _, q := range ds.Questions {
		t0 := time.Now()
		msgs, err := hist.Load(ctx, q.ConversationID, history.Budget{MaxTokens: opts.CompactTokenBudget})
		loadLats = append(loadLats, time.Since(t0))
		if err != nil {
			r.Errors++
			continue
		}

		body := formatHistory(msgs) + "\n\nQUESTION: " + q.Query
		prompt := fmt.Sprintf(opts.AnswerPrompt, body)
		tokens = append(tokens, counter.Count(prompt))

		resp, _, err := opts.AnswerLLM.Generate(ctx, []llm.Message{
			{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: prompt}}},
		})
		if err != nil {
			r.Errors++
			continue
		}
		pred := resp.Content()
		score, err := opts.Judge.Score(ctx, q.Query, pred, q.GoldAnswers)
		if err != nil {
			r.Errors++
			continue
		}
		sumJudge += score
		sumF1 += metrics.F1(pred, q.GoldAnswers)
		if metrics.ExactMatch(pred, q.GoldAnswers) {
			sumEM++
		}
	}

	n := len(ds.Questions) - r.Errors
	if n < 1 {
		n = 1
	}
	r.N = n
	r.Judge = sumJudge / float64(n)
	r.F1 = sumF1 / float64(n)
	r.EM = sumEM / float64(n)
	r.PromptTokensP50, r.PromptTokensP95, r.PromptTokensMax = tokenStats(tokens)
	r.LoadLatencyP50, r.LoadLatencyP95 = latencyPercentiles(loadLats)
	return r, nil
}

// buildHistory returns the History impl for the strategy plus an ingest
// function that pre-loads each conversation. The compactor variant uses an
// in-memory workspace so the bench is hermetic.
func buildHistory(s Strategy, opts Options) (history.History, func(context.Context, dataset.Conversation) error, error) {
	switch s {
	case StrategyNone, StrategyBuffer:
		max := opts.BufferMax
		if s == StrategyNone {
			max = 1 << 30 // unbounded
		}
		h := history.NewBuffer(history.NewInMemoryStore(), history.WithBufferMax(max))
		return h, ingestVerbatim(h), nil
	case StrategyCompacted:
		ws := workspace.NewMemWorkspace()
		h := history.NewCompacted(history.NewInMemoryStore(), opts.SummaryLLM, ws,
			history.WithTokenBudget(opts.CompactTokenBudget),
		)
		return h, ingestVerbatim(h), nil
	default:
		return nil, nil, fmt.Errorf("unknown strategy %q", s)
	}
}

func ingestVerbatim(h history.History) func(context.Context, dataset.Conversation) error {
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
