// Package locomo also exposes the orchestration entry point Run, used by
// `cmd/eval` and unit tests.
package locomo

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/eval/metrics"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

// ingestRetryDelay is the cool-off before the single NotAvailable retry.
// Sized for typical Azure MaaS cold-start / capacity blips (a few seconds)
// without dragging out a sustained outage — at 2 s a full backoff bucket
// of 18-ish 404s in a 50-conv c50 run adds ~40 s wall, negligible next to
// per-batch ingest time of ~2 min.
const ingestRetryDelay = 2 * time.Second

// IngestSaver is implemented by runners that can ingest verbatim turns
// (the default Flowcraft runner exposes SaveRaw to bypass an LLM extractor for
// CI-friendly runs without API keys).
type IngestSaver interface {
	SaveRaw(ctx context.Context, scope recall.Scope, msgs []llm.Message) (saveCount int, saveLatency time.Duration, err error)
}

// Report aggregates one full evaluation run.
type Report struct {
	Runner      string                            `json:"runner"`
	Dataset     string                            `json:"dataset"`
	N           int                               `json:"n"`
	Aggregate   ScoreAggregate                    `json:"aggregate"`
	PerQuestion []QuestionScore                   `json:"per_question"`
	Latency     map[string]metrics.LatencySummary `json:"latency"`
	StartedAt   time.Time                         `json:"started_at"`
	FinishedAt  time.Time                         `json:"finished_at"`
}

// ScoreAggregate is the headline numbers (qa.em, qa.f1, qa.judge, recall.k_hit).
//
// KHitRate is a pointer so it can encode "not applicable" as JSON null — the
// metric compares retrieval IDs against the dataset's evidence_ids and only
// makes sense on raw-ingest runs where the extractor is bypassed. LLM-extractor
// runs mint synthetic fact IDs that have no correspondence to dia_id, so
// including a 0.0 there is misleading.
type ScoreAggregate struct {
	EM       float64  `json:"qa.em"`
	F1       float64  `json:"qa.f1"`
	Judge    float64  `json:"qa.judge"`
	KHitRate *float64 `json:"recall.k_hit,omitempty"`
}

// QuestionScore is one question's per-metric breakdown.
//
// KHit mirrors ScoreAggregate.KHitRate: nil means "not applicable under this
// run's ingest mode" rather than "retrieval failed".
type QuestionScore struct {
	ID         string   `json:"id"`
	Query      string   `json:"query"`
	Prediction string   `json:"prediction"`
	EM         float64  `json:"em"`
	F1         float64  `json:"f1"`
	Judge      float64  `json:"judge"`
	KHit       *float64 `json:"k_hit,omitempty"`
}

// Options controls the evaluation behavior.
type Options struct {
	TopK              int
	Judge             metrics.Judge // nil → EMJudge fallback
	UseExtractor      bool          // true → Save (LLM extractor); false → SaveRaw fallback
	AnswerLLM         llm.LLM       // optional; when set, prediction = LLM(query | top-k hits) instead of raw concat
	AnswerPrompt      string        // optional template; %s receives "Q: …\nMEMORIES:\n- …" — default below
	Concurrency       int           // QA-loop parallelism; defaults to 1 (sequential). Recall/Index is goroutine-safe.
	IngestConcurrency int           // per-conversation extractor batch parallelism; defaults to 1. LTM Save is goroutine-safe; raising this from 1→8 cuts ingest from ~2h to ~15m on LoCoMo10 with qwen-flash.
	ProgressEvery     int           // log every N completed questions; 0 disables.
	IngestTimeout     time.Duration // per-conversation Save() deadline; 0 disables. LLM extractors occasionally hang on Qwen — without this a single bad call wedges the entire run.
	QATimeout         time.Duration // per-question recall+answer+judge deadline; 0 disables.
	RuntimeID         string
	UserID            string
	AgentID           string

	// Hook is invoked at lifecycle checkpoints (start, ingest_done,
	// qa_progress milestones, done, error). The hook is a synchronous
	// best-effort call — backends like Feishu webhooks add ~100ms each
	// time, so we only fire it at coarse-grained events. Set to nil to
	// disable notifications.
	Hook EventHook

	// ProgressPct controls the milestone resolution in percent (e.g. 25
	// fires the hook at 25%, 50%, 75%; 0 disables milestones but still
	// emits start / *_done / error). Must divide 100 cleanly for the
	// "every Nth percent" message to land on exact boundaries.
	ProgressPct int

	// OnQuestionRecall is invoked synchronously after every successful
	// Recall in the QA loop, before the answer LLM is called. It
	// enables the --dump-recall diagnostic to capture which facts the
	// retrieval pipeline actually surfaces for each question — the
	// recall-miss vs answer-miss probe complement to the extractor's
	// OnFactsExtracted hook on the ingest side. nil disables.
	//
	// Callback runs in the QA worker goroutine, so it MUST be
	// goroutine-safe when Concurrency > 1.
	OnQuestionRecall func(q dataset.Question, hits []recall.Hit)
}

// Event describes one lifecycle checkpoint. See [EventHook].
//
// Backends (notify.Feishu, etc.) render Title as the headline and Body as a
// multi-line detail block; Fields carries the structured payload for richer
// transports (cards, JSON sinks).
type Event struct {
	Kind   string            // start | ingest_progress | ingest_done | qa_progress | done | error
	Time   time.Time         // event timestamp
	Title  string            // single-line summary
	Body   string            // multi-line details
	Fields map[string]string // structured key/value payload
}

// EventHook is invoked at lifecycle checkpoints. It runs in the caller's
// goroutine and SHOULD not block: a slow hook will stall the eval. Errors
// from hooks are swallowed (the hook itself decides whether to log).
type EventHook func(ctx context.Context, e Event)

// DefaultAnswerPrompt is the closed-book QA prompt fed to the answer
// LLM after Recall returns the top-K memories for a question.
//
// Five rules, each grounded in a real failure pattern from the
// LoCoMo10 run 25871166419 diagnostic (458/1542 failures sampled),
// not in benchmark-tuning intuition:
//
//  1. STRICT GROUNDING — answer from the listed memories only; do not
//     invent facts. Universal closed-book QA contract.
//
//  2. RESTRAINED PARTIAL-INFO INFERENCE — when memories carry partial
//     evidence (a character's general traits, an indirectly implied
//     date), infer the most likely answer and briefly note the
//     inference. This is NOT the same as mem0's "never say I don't
//     know" rule (which fabricates answers when memories are silent).
//     We deliberately allow "I don't know" when memories truly have
//     nothing — see [eval/README.md] anti-cheating discipline.
//
//  3. MIRROR QUESTION FORM — if the question is WHEN, give a date or
//     duration; HOW MANY, a number; YES/NO, lead with yes/no. Mirror
//     the date format used in the question (e.g. "7 May 2023" vs
//     "May 7, 2023"). A real product behaviour, not a judge trick.
//
//  4. CONCISENESS — 1-2 sentences. Hedging language ("it seems",
//     "might be") dilutes accuracy when memories are unambiguous.
//
//  5. CANONICAL NAME RECOGNITION — characters named anywhere in the
//     memory list are NOT "silent topics". If a question asks about
//     such a character, infer from their statements rather than
//     refusing.
//
//  6. DATE QUALIFIER PRESERVATION — when a memory uses a date QUALIFIER
//     ("around", "roughly", "the week before X", "a few years ago",
//     "last summer"), preserve that qualifier rather than computing a
//     precise date. The qualifier carries the speaker's actual
//     epistemic state; converting "a few years ago" to "27 June 2020"
//     fabricates precision. Driven by the 25872581106 cat2 diagnostic
//     where ~30% of temporal failures came from converting relative
//     framings ("the week before 6 July 2023") into wrong absolute
//     dates.
//
// Note on prior art: mem0's MEMORY_ANSWER_PROMPT (Apache 2.0,
// mem0/configs/prompts.py) is shorter and includes a stricter
// "never say no information is found, provide a general response"
// rule. We deliberately do NOT adopt that rule because it shifts
// bench numbers without reflecting real memory quality (per
// eval/README.md anti-cheating discipline §). Rules 3-4 above borrow
// mem0's "clear, concise" intent; rule 2 is our restrained version of
// their anti-IDK behaviour.
const DefaultAnswerPrompt = `You are answering a question using only the MEMORIES below.

Guidelines:
- Ground the answer strictly in the memories. Do not invent facts that are not supported.
- When the memories carry partial evidence that lets you reasonably infer the answer (e.g. a character's general traits, an indirectly implied date), do so and briefly note the inference. Characters whose names appear in the memories are NEVER "silent topics" — infer from their statements rather than refusing. Reply "I don't know" only when the memories are genuinely silent on the topic.
- Match the form of the question. If asked WHEN, give a specific date or duration; HOW MANY, a number; YES/NO, lead with yes/no.
- Mirror the date format used in the question (e.g. if asked "7 May 2023", answer in that format, not "May 7, 2023").
- If a memory uses a date QUALIFIER ("around", "roughly", "the week before X", "a few years ago", "last summer", "two weekends ago"), preserve that qualifier in your answer rather than computing a precise absolute date. The qualifier carries the speaker's actual epistemic state — fabricating precision is worse than mirroring vagueness.
- Answer in 1-2 sentences. Avoid hedging ("it seems", "might be") when the memories are unambiguous.

%s

Answer:`

// Run runs ingest + question loop and returns a report.
func Run(ctx context.Context, r runners.Runner, ds *dataset.Dataset, opts Options) (*Report, error) {
	if opts.TopK == 0 {
		opts.TopK = 10
	}
	if opts.Judge == nil {
		opts.Judge = metrics.EMJudge{}
	}
	if opts.RuntimeID == "" {
		opts.RuntimeID = "locomo"
	}
	if opts.UserID == "" {
		opts.UserID = "u-bench"
	}
	if opts.AgentID == "" {
		opts.AgentID = "agent-bench"
	}
	// Per-conversation scopes: each conv gets its own user_id so cross-conv
	// facts can't pollute recall. Without this, conv-N's questions retrieve
	// top-k from a pool of all 10 convs combined and relevant facts get
	// drowned out by other conversations (observed: judge=0.67 on a single
	// conv but 0.17 across 10).
	scopeOf := func(convID string) recall.Scope {
		uid := opts.UserID
		if convID != "" {
			uid = opts.UserID + "::" + convID
		}
		return recall.Scope{RuntimeID: opts.RuntimeID, UserID: uid, AgentID: opts.AgentID}
	}

	report := &Report{
		Runner:    r.Name(),
		Dataset:   ds.Name,
		StartedAt: time.Now(),
		Latency:   map[string]metrics.LatencySummary{},
	}

	emit := func(e Event) {
		if opts.Hook == nil {
			return
		}
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		opts.Hook(ctx, e)
	}

	emit(Event{
		Kind:  "start",
		Title: fmt.Sprintf("eval start: runner=%s dataset=%s", report.Runner, report.Dataset),
		Body: fmt.Sprintf(
			"conversations=%d  questions=%d  topk=%d  extractor=%v  qa_concurrency=%d  ingest_concurrency=%d",
			len(ds.Conversations), len(ds.Questions),
			opts.TopK, opts.UseExtractor, opts.Concurrency, opts.IngestConcurrency,
		),
		Fields: map[string]string{
			"runner":  report.Runner,
			"dataset": report.Dataset,
		},
	})

	// LoCoMo conversations are 13k-28k tokens each (40+ sessions); feeding
	// them whole to an LLM extractor blows past output limits and yields
	// 1-2 facts. Slice by session so each Save sees ~1k-3k tokens, the
	// extractor produces 5-15 atomic facts per chunk, and the per-conv
	// total ends up in the 100-400 range expected by the bench.
	//
	// Across conversations we flatten all (conv, batch) pairs into a single
	// worker pool: different conv scopes are independent, so there is no
	// ordering or visibility constraint. This keeps workers busy even when
	// one conv has fewer sessions than another and cuts the 10-conv ingest
	// wall time from ~20 min (conv-serial / batch-parallel) to ~5 min
	// (fully flat) on LoCoMo10.
	ingestStart := time.Now()
	saveLatencies := ingestFlat(ctx, r, scopeOf, ds.Conversations, opts, emit)
	ingestSummary := metrics.Summarize(saveLatencies)
	emit(Event{
		Kind: "ingest_done",
		Title: fmt.Sprintf("ingest done in %s (%d Save calls)",
			time.Since(ingestStart).Truncate(time.Second), len(saveLatencies)),
		Body: fmt.Sprintf("save.p50=%s save.p95=%s", ingestSummary.P50, ingestSummary.P95),
	})

	scores, recallLatencies, err := evalQuestions(ctx, r, scopeOf, ds.Questions, opts, emit)
	if err != nil {
		emit(Event{Kind: "error", Title: "eval failed during QA", Body: err.Error()})
		return nil, err
	}
	report.PerQuestion = scores
	var sumEM, sumF1, sumJudge, sumKHit float64
	var nKHit int
	for _, s := range scores {
		sumEM += s.EM
		sumF1 += s.F1
		sumJudge += s.Judge
		if s.KHit != nil {
			sumKHit += *s.KHit
			nKHit++
		}
	}

	n := float64(len(ds.Questions))
	if n > 0 {
		report.Aggregate = ScoreAggregate{
			EM:    sumEM / n,
			F1:    sumF1 / n,
			Judge: sumJudge / n,
		}
		if nKHit > 0 {
			avg := sumKHit / float64(nKHit)
			report.Aggregate.KHitRate = &avg
		}
	}
	report.N = len(ds.Questions)
	report.Latency["save"] = metrics.Summarize(saveLatencies)
	report.Latency["recall"] = metrics.Summarize(recallLatencies)
	report.FinishedAt = time.Now()

	khit := "N/A"
	if report.Aggregate.KHitRate != nil {
		khit = fmt.Sprintf("%.3f", *report.Aggregate.KHitRate)
	}
	emit(Event{
		Kind: "done",
		Title: fmt.Sprintf("eval done in %s",
			report.FinishedAt.Sub(report.StartedAt).Truncate(time.Second)),
		Body: fmt.Sprintf(
			"qa.judge=%.3f  qa.f1=%.3f  qa.em=%.3f  recall.k_hit=%s\nsave.p95=%s  recall.p95=%s  n=%d",
			report.Aggregate.Judge, report.Aggregate.F1, report.Aggregate.EM, khit,
			report.Latency["save"].P95, report.Latency["recall"].P95, report.N,
		),
		Fields: map[string]string{
			"qa.judge":     fmt.Sprintf("%.6f", report.Aggregate.Judge),
			"qa.f1":        fmt.Sprintf("%.6f", report.Aggregate.F1),
			"qa.em":        fmt.Sprintf("%.6f", report.Aggregate.EM),
			"recall.k_hit": khit,
			"n":            fmt.Sprintf("%d", report.N),
		},
	})

	return report, nil
}

// buildPrediction picks between two answer strategies:
//   - opts.AnswerLLM != nil → ask the LLM to answer the question grounded in
//     the recalled memories (closed-book QA over LTM).
//   - otherwise              → cheap fallback: concatenate top-3 hits, so
//     EM/F1 still surface a "did retrieval find the right text" signal.
func buildPrediction(ctx context.Context, opts Options, query string, hits []recall.Hit) (string, error) {
	if opts.AnswerLLM == nil {
		return composePrediction(hits), nil
	}
	prompt := opts.AnswerPrompt
	if prompt == "" {
		prompt = DefaultAnswerPrompt
	}
	body := buildAnswerBody(query, hits)
	resp, _, err := opts.AnswerLLM.Generate(ctx, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: fmt.Sprintf(prompt, body)}}},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content()), nil
}

// buildAnswerBody renders the "Q + MEMORIES" block fed into the QA prompt.
// Top-k memories are listed as bullets; ordering matches RecallHit ranking.
func buildAnswerBody(query string, hits []recall.Hit) string {
	var b strings.Builder
	b.WriteString("QUESTION: ")
	b.WriteString(query)
	b.WriteString("\n\nMEMORIES:\n")
	if len(hits) == 0 {
		b.WriteString("(none)\n")
		return b.String()
	}
	for _, h := range hits {
		b.WriteString("- ")
		b.WriteString(strings.ReplaceAll(h.Entry.Content, "\n", " "))
		b.WriteString("\n")
	}
	return b.String()
}

// composePrediction concatenates the top-3 hit contents — the "answer" we feed
// to EM/F1/Judge when no AnswerLLM is configured. Cheap, deterministic, and
// good enough to surface "did retrieval find the right text" without an API key.
func composePrediction(hits []recall.Hit) string {
	if len(hits) == 0 {
		return ""
	}
	max := 3
	if max > len(hits) {
		max = len(hits)
	}
	var b strings.Builder
	for i := 0; i < max; i++ {
		if i > 0 {
			b.WriteString(" || ")
		}
		b.WriteString(hits[i].Entry.Content)
	}
	return b.String()
}

func evidenceKHit(hits []recall.Hit, want []string) float64 {
	if len(want) == 0 {
		return 0
	}
	got := map[string]struct{}{}
	for _, h := range hits {
		got[h.Entry.ID] = struct{}{}
	}
	for _, w := range want {
		if _, ok := got[w]; ok {
			return 1
		}
	}
	return 0
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func convoToMessages(c dataset.Conversation) []llm.Message {
	out := make([]llm.Message, 0, len(c.Turns))
	for _, t := range c.Turns {
		role := model.Role(t.Role)
		out = append(out, llm.Message{Role: role, Parts: []model.Part{{Type: model.PartText, Text: t.Content}}})
	}
	return out
}

// evalQuestions runs the QA loop with bounded concurrency. Recall, answer
// synthesis and judge are all per-question pure functions (modulo LLM calls),
// so we fan them out across N workers; results are collected back in dataset
// order so the per_question array stays stable across runs.
// perQuestionCtx returns a child context with the given deadline, or the
// parent unchanged when timeout <= 0. The returned cancel is always safe
// to call (no-op when no deadline was attached) so workers can call it
// once per question without conditionals.
func perQuestionCtx(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

// ingestJob carries one Save unit: a single session-sliced batch of turns
// tagged with its owning conversation. All jobs across all conversations
// are drained by a single worker pool so workers stay busy even when one
// conversation has fewer sessions than another.
type ingestJob struct {
	convID    string
	scope     recall.Scope
	batch     turnBatch
	batchIdx  int // 0-based position within its conversation, for WARN logs
	convTotal int // total batches for the owning conversation, for WARN logs
}

// ingestFlat runs every (conversation, batch) pair through a single worker
// pool. LTM Save is goroutine-safe within a scope and conversations use
// distinct scopes, so there are no cross-batch ordering constraints.
//
// The pool size is IngestConcurrency (global, not per-conversation) — with
// 16 workers and ~250 batches total on LoCoMo10 the ingest phase drops from
// ~20 min to ~5 min while keeping provider-side pressure bounded.
//
// Progress is logged per-conversation: a counter tracks remaining batches
// per conv and a completion line is emitted when the last batch of a conv
// finishes (order is non-deterministic across conversations).
//
// Failures on any single batch are logged + skipped so one rate-limited or
// truncated extractor response can't disqualify an entire conversation.
//
// emit is a checkpoint callback that fires at coarse-grained progress
// boundaries (controlled by opts.ProgressPct). Passing a no-op closure
// disables notifications without touching the inner loop.
func ingestFlat(ctx context.Context, r runners.Runner, scopeOf func(string) recall.Scope, convs []dataset.Conversation, opts Options, emit func(Event)) []time.Duration {
	// Precompute conversation-level counters + expand every conv into its
	// batches. Keeping convStart out of the worker path means no
	// synchronization is needed for timing.
	type convAgg struct {
		start     time.Time
		remaining int
		facts     int
		total     int
	}
	aggs := make(map[string]*convAgg, len(convs))
	var jobs []ingestJob
	for _, c := range convs {
		batches := batchTurnsBySession(c)
		if len(batches) == 0 {
			continue
		}
		scope := scopeOf(c.ID)
		aggs[c.ID] = &convAgg{start: time.Now(), remaining: len(batches), total: len(batches)}
		for bi, b := range batches {
			jobs = append(jobs, ingestJob{
				convID:    c.ID,
				scope:     scope,
				batch:     b,
				batchIdx:  bi,
				convTotal: len(batches),
			})
		}
	}
	if len(jobs) == 0 {
		return nil
	}

	conc := opts.IngestConcurrency
	if conc <= 0 {
		conc = 1
	}
	if conc > len(jobs) {
		conc = len(jobs)
	}

	var (
		mu          sync.Mutex
		latencies   []time.Duration
		done        int
		totalFacts  int
		nextPctMark int
		jobCh       = make(chan ingestJob)
		wg          sync.WaitGroup
	)
	totalJobs := len(jobs)
	if opts.ProgressPct > 0 {
		nextPctMark = opts.ProgressPct
	}
	ingestStarted := time.Now()

	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				// attempt does one Save dispatch with a fresh per-batch
				// context. Pulled into a closure so the NotAvailable
				// retry below reuses the exact same provider/extractor
				// switch without duplicating it.
				attempt := func() (int, time.Duration, error) {
					ingestCtx, cancel := perQuestionCtx(ctx, opts.IngestTimeout)
					defer cancel()
					if !opts.UseExtractor {
						switch rs := r.(type) {
						case runners.RawIngestSaver:
							return rs.SaveRawTurns(ingestCtx, job.scope, job.batch.rawTurns)
						case IngestSaver:
							return rs.SaveRaw(ingestCtx, job.scope, job.batch.msgs)
						}
					}
					return r.Save(ingestCtx, job.scope, job.batch.msgs)
				}

				n, d, err := attempt()
				// Single-shot retry on transient provider errors
				// (errdefs.NotAvailable = 5xx, network flake, plus
				// Azure MaaS capacity 404s that ClassifyProviderError
				// falls through to the default bucket). One retry only
				// — it's cheap, recovers the common Azure cold-start
				// blip seen on the c50-azure run, and won't mask a
				// sustained outage.
				if err != nil && errdefs.IsNotAvailable(err) {
					log.Printf("[locomo] retry ingest %s/%s (batch %d/%d): %v", job.convID, job.batch.session, job.batchIdx+1, job.convTotal, err)
					select {
					case <-ctx.Done():
					case <-time.After(ingestRetryDelay):
					}
					if ctx.Err() == nil {
						n, d, err = attempt()
					}
				}

				mu.Lock()
				done++
				a := aggs[job.convID]
				a.remaining--
				if err != nil {
					log.Printf("[locomo] WARN ingest %s/%s (batch %d/%d, overall %d/%d): %v", job.convID, job.batch.session, job.batchIdx+1, job.convTotal, done, totalJobs, err)
				} else {
					latencies = append(latencies, d)
					a.facts += n
					totalFacts += n
				}
				// Per-batch heartbeat: without this the run looks frozen for
				// 5-15 min on slow extractor models because the only previous
				// log point was per-conversation completion. Now the user
				// gets a line every Save call (success or failure) so they
				// can spot rate-limit walls or hung calls early.
				if opts.ProgressEvery > 0 {
					if err == nil {
						log.Printf("[locomo] ingest %s/%s batch %d/%d in %s, %d facts (overall %d/%d)", job.convID, job.batch.session, job.batchIdx+1, job.convTotal, d.Truncate(100*time.Millisecond), n, done, totalJobs)
					}
				}
				if a.remaining == 0 {
					log.Printf("[locomo] ingest %s done in %s, %d facts saved (%d batches, overall %d/%d)", job.convID, time.Since(a.start).Truncate(time.Second), a.facts, a.total, done, totalJobs)
				}
				// Milestone notification (e.g. every 25%): emit on the
				// completing worker so we don't need a separate goroutine.
				// Done count is monotonic and protected by mu, so the
				// first worker to cross the boundary fires exactly once.
				var milestone *Event
				if nextPctMark > 0 && totalJobs > 0 {
					pct := done * 100 / totalJobs
					if pct >= nextPctMark && pct < 100 {
						milestone = &Event{
							Kind: "ingest_progress",
							Title: fmt.Sprintf("ingest %d%% (%d/%d batches)",
								pct, done, totalJobs),
							Body: fmt.Sprintf("facts=%d  elapsed=%s",
								totalFacts, time.Since(ingestStarted).Truncate(time.Second)),
						}
						for nextPctMark <= pct {
							nextPctMark += opts.ProgressPct
						}
					}
				}
				mu.Unlock()
				if milestone != nil {
					emit(*milestone)
				}
			}
		}()
	}

	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)
	wg.Wait()
	return latencies
}

func evalQuestions(ctx context.Context, r runners.Runner, scopeOf func(string) recall.Scope, qs []dataset.Question, opts Options, emit func(Event)) ([]QuestionScore, []time.Duration, error) {
	n := len(qs)
	scores := make([]QuestionScore, n)
	latencies := make([]time.Duration, n)

	conc := opts.Concurrency
	if conc <= 0 {
		conc = 1
	}
	if conc > n {
		conc = n
	}

	type job struct{ idx int }
	jobs := make(chan job)
	var done, failed atomic.Int64
	// nextPctMark gates milestone notifications. Bumped atomically by
	// whichever worker first crosses each threshold.
	var nextPctMark atomic.Int64
	if opts.ProgressPct > 0 {
		nextPctMark.Store(int64(opts.ProgressPct))
	}
	qaStart := time.Now()
	maybeMilestone := func(cur int64) {
		mark := nextPctMark.Load()
		if mark <= 0 || n == 0 {
			return
		}
		pct := cur * 100 / int64(n)
		if pct < mark || pct >= 100 {
			return
		}
		// Try to claim the milestone; only one worker wins per boundary.
		next := mark
		for next <= pct {
			next += int64(opts.ProgressPct)
		}
		if !nextPctMark.CompareAndSwap(mark, next) {
			return
		}
		emit(Event{
			Kind: "qa_progress",
			Title: fmt.Sprintf("qa %d%% (%d/%d questions)",
				pct, cur, n),
			Body: fmt.Sprintf("failed=%d  elapsed=%s",
				failed.Load(), time.Since(qaStart).Truncate(time.Second)),
		})
	}

	// recordFailure scores a question as 0/0/0 and logs the cause. We do
	// NOT fail the whole run on a single LLM error: Azure content filters,
	// Qwen rate limits and per-question timeouts are common enough that
	// a 1-in-1500 question reliably trips one — propagating that as an
	// exit-1 throws away the other 1499 results.
	// alertedHighFailure fires the "systemic failure" notification at most
	// once per run: once we cross the 5% threshold, every subsequent failure
	// still keeps the ratio bad, so resending would spam the webhook.
	var alertedHighFailure atomic.Bool
	recordFailure := func(idx int, q dataset.Question, stage string, err error) {
		log.Printf("[locomo] WARN %s %s: %v (scored 0)", stage, q.ID, err)
		var khitPtr *float64
		if !opts.UseExtractor {
			z := 0.0
			khitPtr = &z
		}
		scores[idx] = QuestionScore{
			ID: q.ID, Query: q.Query, Prediction: "",
			EM: 0, F1: 0, Judge: 0, KHit: khitPtr,
		}
		f := failed.Add(1)
		// Hard-failure threshold: if more than 5% of QA fail after the
		// first 100 questions, something systemic is broken (bad
		// credentials, dead provider). Notify once so the operator can
		// stop the run instead of burning 50h producing zeros.
		if d := done.Load() + 1; d >= 100 && f*20 > d && alertedHighFailure.CompareAndSwap(false, true) {
			emit(Event{
				Kind:  "error",
				Title: fmt.Sprintf("qa failure rate %.0f%% on first %d questions", float64(f)/float64(d)*100, d),
				Body:  fmt.Sprintf("most recent: %s on %s — %v\n(further failures will be logged but not re-notified)", stage, q.ID, err),
			})
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				q := qs[j.idx]
				qctx, cancel := perQuestionCtx(ctx, opts.QATimeout)
				hits, d, err := r.Recall(qctx, scopeOf(q.ConversationID), q.Query, opts.TopK)
				if err != nil {
					cancel()
					recordFailure(j.idx, q, "recall", err)
					maybeMilestone(done.Add(1))
					continue
				}
				latencies[j.idx] = d
				if opts.OnQuestionRecall != nil {
					opts.OnQuestionRecall(q, hits)
				}
				pred, err := buildPrediction(qctx, opts, q.Query, hits)
				if err != nil {
					cancel()
					recordFailure(j.idx, q, "answer", err)
					maybeMilestone(done.Add(1))
					continue
				}
				em := boolFloat(metrics.ExactMatch(pred, q.GoldAnswers))
				f1 := metrics.F1(pred, q.GoldAnswers)
				judge, err := opts.Judge.Score(qctx, q.Query, pred, q.GoldAnswers)
				cancel()
				if err != nil {
					recordFailure(j.idx, q, "judge", err)
					maybeMilestone(done.Add(1))
					continue
				}
				// k_hit only applies on raw-ingest runs: MemoryEntry.ID is the
				// dia_id so we can check evidence overlap. In extractor mode
				// the ID is a freshly-minted ULID with no provenance back to
				// dia_id, so any comparison returns 0 — we record nil so the
				// aggregate reports N/A instead of a misleading 0.000.
				var khitPtr *float64
				if !opts.UseExtractor {
					khit := evidenceKHit(hits, q.EvidenceIDs)
					khitPtr = &khit
				}
				scores[j.idx] = QuestionScore{
					ID: q.ID, Query: q.Query, Prediction: pred,
					EM: em, F1: f1, Judge: judge, KHit: khitPtr,
				}
				cur := done.Add(1)
				if opts.ProgressEvery > 0 && cur%int64(opts.ProgressEvery) == 0 {
					log.Printf("[locomo] %d/%d questions done", cur, n)
				}
				maybeMilestone(cur)
			}
		}()
	}

	for i := 0; i < n; i++ {
		jobs <- job{idx: i}
	}
	close(jobs)
	wg.Wait()
	if f := failed.Load(); f > 0 {
		log.Printf("[locomo] %d/%d questions failed (scored 0); see WARN logs above", f, n)
	}
	return scores, latencies, nil
}

// convoToRawTurns mirrors convoToMessages but preserves each turn's
// upstream EvidenceID for runners that support it.
func convoToRawTurns(c dataset.Conversation) []runners.RawTurn {
	out := make([]runners.RawTurn, 0, len(c.Turns))
	for _, t := range c.Turns {
		out = append(out, runners.RawTurn{
			Role:       t.Role,
			Content:    t.Content,
			EvidenceID: t.EvidenceID,
		})
	}
	return out
}

// turnBatch groups a contiguous slice of one conversation's turns by their
// SessionID. Both shapes (LLM messages + raw turns) are precomputed so the
// ingest loop can dispatch to either Runner variant without re-iterating.
type turnBatch struct {
	session  string
	msgs     []llm.Message
	rawTurns []runners.RawTurn
}

// batchTurnsBySession splits a Conversation by Turn.SessionID, preserving
// turn order. Turns without SessionID fall into a single "" bucket — that
// preserves backward compatibility with datasets that don't record session
// boundaries (the result is one whole-conv batch, like the old behavior).
func batchTurnsBySession(c dataset.Conversation) []turnBatch {
	if len(c.Turns) == 0 {
		return nil
	}
	var batches []turnBatch
	cur := turnBatch{session: c.Turns[0].SessionID}
	for _, t := range c.Turns {
		if t.SessionID != cur.session {
			batches = append(batches, cur)
			cur = turnBatch{session: t.SessionID}
		}
		cur.msgs = append(cur.msgs, llm.Message{
			Role:  model.Role(t.Role),
			Parts: []model.Part{{Type: model.PartText, Text: t.Content}},
		})
		cur.rawTurns = append(cur.rawTurns, runners.RawTurn{
			Role:       t.Role,
			Content:    t.Content,
			EvidenceID: t.EvidenceID,
		})
	}
	batches = append(batches, cur)
	return batches
}
