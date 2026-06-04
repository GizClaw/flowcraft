// Package locomo also exposes the orchestration entry point Run, used by
// `cmd/eval` and unit tests.
package locomo

import (
	"context"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
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
)

// ingestRetryDelay is the cool-off before the single NotAvailable retry.
// Sized for typical Azure MaaS cold-start / capacity blips (a few seconds)
// without dragging out a sustained outage — at 2 s a full backoff bucket
// of 18-ish 404s in a 50-conv c50 run adds ~40 s wall, negligible next to
// per-batch ingest time of ~2 min.
const ingestRetryDelay = 2 * time.Second

// qaRetryDelay mirrors ingestRetryDelay for the recall / answer / judge
// stages of the QA loop. A real-world DS-Flash run on c50 showed ~3 % of
// LLM calls flapping with transient 404s; without retry every flap landed
// in recordFailure and was scored 0, contaminating the qa.judge headline.
// The cost analysis is the same as ingest's: 2 s × ~5 % flap rate × 1500
// questions ≈ 150 s extra wall on a ~30 min QA loop, well under a 10 %
// slowdown and dwarfed by the value of not losing 50+ questions to
// provider transients. We deliberately use the same 2 s constant as
// ingest so future operators see one retry budget, not two.
const qaRetryDelay = 2 * time.Second

// retryOnNotAvailable runs attempt once; if it returns a NotAvailable
// error (5xx, network flake, Azure MaaS capacity 404 — see errdefs.go's
// ClassifyProviderError default bucket) it waits qaRetryDelay then tries
// once more. Single-shot to recover transient blips without masking
// sustained outages. Shares the question's existing context: if the
// first attempt burned most of the QA budget, the retry inherits the
// remainder, which is the desired bound for a per-question SLA.
func retryOnNotAvailable(ctx context.Context, stage, qid string, attempt func() error) error {
	err := attempt()
	if err == nil || !errdefs.IsNotAvailable(err) {
		return err
	}
	log.Printf("[locomo] retry %s %s: %v", stage, qid, err)
	select {
	case <-ctx.Done():
		return err
	case <-time.After(qaRetryDelay):
	}
	if ctx.Err() != nil {
		return err
	}
	return attempt()
}

// IngestSaver is implemented by runners that can ingest verbatim turns
// (the default Flowcraft runner exposes SaveRaw to bypass an LLM extractor for
// CI-friendly runs without API keys).
type IngestSaver interface {
	SaveRaw(ctx context.Context, scope runners.Scope, msgs []llm.Message) (saveCount int, saveLatency time.Duration, err error)
}

// Report aggregates one full evaluation run.
type Report struct {
	Runner           string                            `json:"runner"`
	RecallVersion    string                            `json:"recall_version,omitempty"`
	Baseline         string                            `json:"baseline,omitempty"`
	RetrievalBackend string                            `json:"retrieval_backend,omitempty"`
	Dataset          string                            `json:"dataset"`
	Source           map[string]string                 `json:"source,omitempty"`
	N                int                               `json:"n"`
	Ingest           *IngestSummary                    `json:"ingest,omitempty"`
	Aggregate        ScoreAggregate                    `json:"aggregate"`
	PerQuestion      []QuestionScore                   `json:"per_question"`
	Latency          map[string]metrics.LatencySummary `json:"latency"`
	StartedAt        time.Time                         `json:"started_at"`
	FinishedAt       time.Time                         `json:"finished_at"`
}

// IngestSummary records Save batch outcomes. Failed batches are skipped by the
// eval loop, so surfacing them in the report keeps provider filters/timeouts
// from being mistaken for normal extraction quality.
type IngestSummary struct {
	AttemptedBatches int             `json:"attempted_batches"`
	SucceededBatches int             `json:"succeeded_batches"`
	FailedBatches    int             `json:"failed_batches"`
	SavedFacts       int             `json:"saved_facts"`
	SaveLatencies    []time.Duration `json:"-"`
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

	// ByCategory carries per-question-category breakdowns of the
	// headline metrics. The key is one of the canonical LoCoMo labels
	// emitted by the converter ("single-hop", "temporal", "multi-hop",
	// "open-domain", "adversarial"); raw `catN` tags are intentionally
	// skipped here so the map shape stays stable across upstream
	// re-numbering changes. Only present when the dataset's questions
	// carry tags; left nil otherwise so legacy datasets keep their
	// previous report shape.
	ByCategory map[string]CategoryScore `json:"byCategory,omitempty"`
}

// CategoryScore is one category's slice of the headline metrics. Count
// is the number of questions matching the category tag; the metric
// fields are means over those questions (NOT over the whole dataset).
type CategoryScore struct {
	Count int     `json:"count"`
	EM    float64 `json:"qa.em"`
	F1    float64 `json:"qa.f1"`
	Judge float64 `json:"qa.judge"`
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

	// Tags carries the question's category tags from the source dataset
	// (e.g. "cat1", "single-hop"). Propagated unchanged so downstream
	// per-category aggregation works on either the canonical names or
	// raw catN — see ScoreAggregate.ByCategory.
	Tags []string `json:"tags,omitempty"`
}

// Options controls the evaluation behavior.
type Options struct {
	TopK              int
	Judge             metrics.Judge // nil → EMJudge fallback
	UseExtractor      bool          // true → Save (LLM extractor); false → SaveRaw fallback
	SkipIngest        bool          // true → assume the runner was preloaded and run QA only
	AnswerLLM         llm.LLM       // optional; when set, prediction = LLM(query | top-k hits) instead of raw concat
	AnswerPrompt      string        // optional template; %s receives "Q: …\nMEMORIES:\n- …" — default below
	Concurrency       int           // QA-loop parallelism; defaults to 1 (sequential). Recall/Index is goroutine-safe.
	IngestConcurrency int           // conversation-level ingest parallelism; defaults to 1. Each conversation's session batches are saved sequentially to preserve write-time causality.
	ProgressEvery     int           // log every N completed questions; 0 disables.
	IngestTimeout     time.Duration // per-conversation Save() deadline; 0 disables. LLM extractors occasionally hang on Qwen — without this a single bad call wedges the entire run.
	QATimeout         time.Duration // per-question recall+answer+judge deadline; 0 disables.
	RuntimeID         string
	UserID            string
	AgentID           string
	// RetrievalBackend is a display label for lifecycle notifications. It
	// keeps Feishu cards distinguishable when the same canonical runner is
	// executed against different retrieval indexes (for example v1+memory vs
	// v1+bbh) without changing the report's runner identity.
	RetrievalBackend string
	// RunName is a display/debug identifier for lifecycle notifications. The
	// CLI wires this from --notify-name so Feishu card bodies can be traced
	// back to the exact workflow run or ad-hoc command that created them.
	RunName string

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
	// enables the --dump-recall diagnostic to capture which artifacts the
	// retrieval pipeline actually surfaces for each question — the
	// recall-miss vs answer-miss probe complement to the extractor's
	// OnFactsExtracted hook on the ingest side. nil disables.
	//
	// Callback runs in the QA worker goroutine, so it MUST be
	// goroutine-safe when Concurrency > 1.
	OnQuestionRecall func(q dataset.Question, artifacts []runners.RecallArtifact)
	// OnQuestionRecallStageAudit receives per-stage read-pipeline
	// candidate snapshots when the runner supports them.
	OnQuestionRecallStageAudit func(q dataset.Question, audit runners.RecallStageAudit)
	// OnQuestionAnswer receives the exact answer prompt/body and scored
	// outcome after a successful answer+judge pair. It is diagnostic-only:
	// callbacks must not mutate hits or scores.
	OnQuestionAnswer func(AnswerReplayRecord)
	// OnIngestError receives failed Save batches so diagnostics like
	// --dump-facts can record extract failures that never reach runner hooks.
	// It is called while the ingest progress mutex is held, so callbacks should
	// be lightweight and must not call back into Run.
	OnIngestError func(scope runners.Scope, convID string, batch turnBatch, batchNumber, batchTotal int, err error)
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

func startDebugFields(runName string) map[string]string {
	fields := map[string]string{
		"pid": strconv.Itoa(os.Getpid()),
	}
	if runName != "" {
		fields["run"] = runName
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		fields["host"] = host
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		fields["cwd"] = cwd
	}
	if git := buildRevision(); git != "" {
		fields["git"] = git
	}
	return fields
}

func buildRevision() string {
	if sha := os.Getenv("GITHUB_SHA"); sha != "" {
		return shortRevision(sha)
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return gitCommandRevision()
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = shortRevision(setting.Value)
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision == "" {
		return gitCommandRevision()
	}
	if modified == "true" {
		return revision + "-dirty"
	}
	return revision
}

func gitCommandRevision() string {
	out, err := exec.Command("git", "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return ""
	}
	revision := strings.TrimSpace(string(out))
	if revision == "" {
		return ""
	}
	if err := exec.Command("git", "diff-index", "--quiet", "HEAD", "--").Run(); err != nil {
		return revision + "-dirty"
	}
	return revision
}

func shortRevision(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func formatStartDebug(fields map[string]string) string {
	ordered := []string{"run", "host", "pid", "cwd", "git"}
	parts := make([]string, 0, len(fields))
	for _, key := range ordered {
		if fields[key] != "" {
			parts = append(parts, key+"="+fields[key])
		}
	}
	for key, value := range fields {
		if value == "" {
			continue
		}
		found := false
		for _, orderedKey := range ordered {
			if key == orderedKey {
				found = true
				break
			}
		}
		if !found {
			parts = append(parts, key+"="+value)
		}
	}
	return "source " + strings.Join(parts, "  ")
}

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
	scopeOf := func(convID string) runners.Scope {
		uid := opts.UserID
		if convID != "" {
			uid = opts.UserID + "::" + convID
		}
		return runners.Scope{RuntimeID: opts.RuntimeID, UserID: uid, AgentID: opts.AgentID}
	}

	startDebug := startDebugFields(opts.RunName)
	report := &Report{
		Runner:           r.Name(),
		RetrievalBackend: opts.RetrievalBackend,
		Dataset:          ds.Name,
		Source:           startDebug,
		StartedAt:        time.Now(),
		Latency:          map[string]metrics.LatencySummary{},
	}
	switch report.Runner {
	case runnerFlowcraftRecallV2, "flowcraft-v2":
		report.RecallVersion = "v2"
		report.Baseline = "bootstrap-raw"
	default:
		report.RecallVersion = "v1"
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

	startFields := map[string]string{
		"runner":  report.Runner,
		"dataset": report.Dataset,
	}
	maps.Copy(startFields, startDebug)
	startTitle := fmt.Sprintf("eval start: runner=%s dataset=%s", report.Runner, report.Dataset)
	if opts.RetrievalBackend != "" {
		startFields["retrieval_backend"] = opts.RetrievalBackend
		startTitle = fmt.Sprintf("eval start: runner=%s retrieval=%s dataset=%s", report.Runner, opts.RetrievalBackend, report.Dataset)
	}
	if opts.RunName != "" {
		if opts.RetrievalBackend != "" {
			startTitle = fmt.Sprintf("eval start: run=%s runner=%s retrieval=%s dataset=%s", opts.RunName, report.Runner, opts.RetrievalBackend, report.Dataset)
		} else {
			startTitle = fmt.Sprintf("eval start: run=%s runner=%s dataset=%s", opts.RunName, report.Runner, report.Dataset)
		}
	}
	emit(Event{
		Kind:  "start",
		Title: startTitle,
		Body: fmt.Sprintf(
			"conversations=%d  questions=%d  topk=%d  extractor=%v  qa_concurrency=%d  ingest_concurrency=%d\n%s",
			len(ds.Conversations), len(ds.Questions),
			opts.TopK, opts.UseExtractor, opts.Concurrency, opts.IngestConcurrency,
			formatStartDebug(startDebug),
		),
		Fields: startFields,
	})

	// LoCoMo conversations are 13k-28k tokens each (40+ sessions); feeding
	// them whole to an LLM extractor blows past output limits and yields
	// 1-2 facts. Slice by session so each Save sees ~1k-3k tokens, the
	// extractor produces 5-15 atomic facts per chunk, and the per-conv
	// total ends up in the 100-400 range expected by the bench.
	//
	// Across conversations we use a worker pool, but a worker processes one
	// conversation's session batches in order. This matches online usage more
	// closely: prior sessions in the same conversation are committed before
	// later sessions are extracted, while independent conversations can still
	// ingest in parallel.
	var saveLatencies []time.Duration
	if opts.SkipIngest {
		report.Ingest = &IngestSummary{}
		emit(Event{
			Kind:  "ingest_done",
			Title: "ingest skipped: runner preloaded",
			Body:  "save.p50=0s save.p95=0s",
		})
	} else {
		ingestStart := time.Now()
		ingest := ingestConversations(ctx, r, scopeOf, ds.Conversations, opts, emit)
		report.Ingest = &ingest
		saveLatencies = ingest.SaveLatencies
		ingestSummary := metrics.Summarize(saveLatencies)
		warning := ""
		if ingest.FailedBatches > 0 {
			warning = fmt.Sprintf(" failed_batches=%d", ingest.FailedBatches)
		}
		emit(Event{
			Kind: "ingest_done",
			Title: fmt.Sprintf("ingest done in %s (%d Save calls)",
				time.Since(ingestStart).Truncate(time.Second), ingest.AttemptedBatches),
			Body: fmt.Sprintf("save.p50=%s save.p95=%s saved_facts=%d%s", ingestSummary.P50, ingestSummary.P95, ingest.SavedFacts, warning),
		})
	}

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
		report.Aggregate.ByCategory = aggregateByCategory(scores)
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

// ingestConversationJob carries one conversation's session-sliced Save units.
// A worker processes batches in slice order so the write path observes the same
// temporal sequence a real online app would see for that conversation.
type ingestConversationJob struct {
	convID  string
	scope   runners.Scope
	batches []turnBatch
}

// ingestConversations runs a conversation-level worker pool. Conversations use
// distinct scopes and can ingest concurrently, but session batches inside one
// conversation are saved strictly in order.
//
// The pool size is IngestConcurrency over conversations, not over individual
// Save calls. With 16 workers on LoCoMo10 this caps at the number of
// conversations while preserving per-conversation write-time causality.
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
func ingestConversations(ctx context.Context, r runners.Runner, scopeOf func(string) runners.Scope, convs []dataset.Conversation, opts Options, emit func(Event)) IngestSummary {
	var jobs []ingestConversationJob
	totalBatches := 0
	for _, c := range convs {
		batches := batchTurnsBySession(c)
		if opts.UseExtractor {
			batches = batchTurnsByOnlineSavePoint(c)
		}
		if len(batches) == 0 {
			continue
		}
		jobs = append(jobs, ingestConversationJob{
			convID:  c.ID,
			scope:   scopeOf(c.ID),
			batches: batches,
		})
		totalBatches += len(batches)
	}
	if len(jobs) == 0 {
		return IngestSummary{}
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
		stats       = IngestSummary{AttemptedBatches: totalBatches}
		done        int
		totalFacts  int
		nextPctMark int
		jobCh       = make(chan ingestConversationJob)
		wg          sync.WaitGroup
	)
	if opts.ProgressPct > 0 {
		nextPctMark = opts.ProgressPct
	}
	ingestStarted := time.Now()

	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				convStart := time.Now()
				convFacts := 0
				convTotal := len(job.batches)
				for bi, batch := range job.batches {
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
								return rs.SaveRawTurns(ingestCtx, job.scope, batch.rawTurns)
							case IngestSaver:
								return rs.SaveRaw(ingestCtx, job.scope, batch.msgs)
							}
						}
						if ts, ok := r.(runners.ContextualSourceTurnSaver); ok {
							return ts.SaveSourceTurnsWithContext(ingestCtx, job.scope, batch.rawTurns, batch.recentRawTurns)
						}
						if ts, ok := r.(runners.SourceTurnSaver); ok {
							return ts.SaveSourceTurns(ingestCtx, job.scope, batch.rawTurns)
						}
						return r.Save(ingestCtx, job.scope, batch.msgs)
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
						log.Printf("[locomo] retry ingest %s/%s (batch %d/%d): %v", job.convID, batch.session, bi+1, convTotal, err)
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
					if err != nil {
						stats.FailedBatches++
						log.Printf("[locomo] WARN ingest %s/%s (batch %d/%d, overall %d/%d): %v", job.convID, batch.session, bi+1, convTotal, done, totalBatches, err)
						if opts.OnIngestError != nil {
							opts.OnIngestError(job.scope, job.convID, batch, bi+1, convTotal, err)
						}
					} else {
						stats.SucceededBatches++
						stats.SaveLatencies = append(stats.SaveLatencies, d)
						convFacts += n
						totalFacts += n
						stats.SavedFacts += n
					}
					// Per-batch heartbeat: without this the run looks frozen for
					// 5-15 min on slow extractor models because the only previous
					// log point was per-conversation completion. Now the user
					// gets a line every Save call (success or failure) so they
					// can spot rate-limit walls or hung calls early.
					if opts.ProgressEvery > 0 {
						if err == nil {
							log.Printf("[locomo] ingest %s/%s batch %d/%d in %s, %d facts (overall %d/%d)", job.convID, batch.session, bi+1, convTotal, d.Truncate(100*time.Millisecond), n, done, totalBatches)
						}
					}
					if bi == convTotal-1 {
						log.Printf("[locomo] ingest %s done in %s, %d facts saved (%d batches, overall %d/%d)", job.convID, time.Since(convStart).Truncate(time.Second), convFacts, convTotal, done, totalBatches)
					}
					// Milestone notification (e.g. every 25%): emit on the
					// completing worker so we don't need a separate goroutine.
					// Done count is monotonic and protected by mu, so the
					// first worker to cross the boundary fires exactly once.
					var milestone *Event
					if nextPctMark > 0 && totalBatches > 0 {
						pct := done * 100 / totalBatches
						if pct >= nextPctMark && pct < 100 {
							milestone = &Event{
								Kind: "ingest_progress",
								Title: fmt.Sprintf("ingest %d%% (%d/%d batches)",
									pct, done, totalBatches),
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
			}
		}()
	}

	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)
	wg.Wait()
	return stats
}

func evalQuestions(ctx context.Context, r runners.Runner, scopeOf func(string) runners.Scope, qs []dataset.Question, opts Options, emit func(Event)) ([]QuestionScore, []time.Duration, error) {
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
			Tags: q.Tags,
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
				// Each of the three stages is wrapped in a closure so
				// retryOnNotAvailable can fire it twice on a transient
				// provider blip without us threading the return values
				// through a generic helper. Same single-shot policy as
				// the ingest path.
				var artifacts []runners.RecallArtifact
				var answerContext runners.AnswerContext
				var audit runners.RecallStageAudit
				var d time.Duration
				err := retryOnNotAvailable(qctx, "recall", q.ID, func() error {
					var rerr error
					answerQuestion := runners.AnswerQuestion{Query: q.Query, AskedAt: q.AskedAt}
					if opts.OnQuestionRecallStageAudit != nil {
						if auditor, ok := r.(runners.AnswerContextStageAuditor); ok {
							artifacts, answerContext, audit, d, rerr = auditor.RecallAnswerContextWithStageAudit(qctx, scopeOf(q.ConversationID), answerQuestion, opts.TopK)
							return rerr
						}
						if auditor, ok := r.(runners.RecallStageAuditor); ok {
							artifacts, audit, d, rerr = auditor.RecallWithStageAudit(qctx, scopeOf(q.ConversationID), q.Query, opts.TopK)
							return rerr
						}
					}
					if answerer, ok := r.(runners.AnswerContextRecaller); ok {
						artifacts, answerContext, d, rerr = answerer.RecallAnswerContext(qctx, scopeOf(q.ConversationID), answerQuestion, opts.TopK)
						return rerr
					}
					artifacts, d, rerr = r.Recall(qctx, scopeOf(q.ConversationID), q.Query, opts.TopK)
					return rerr
				})
				if err != nil {
					cancel()
					recordFailure(j.idx, q, "recall", err)
					maybeMilestone(done.Add(1))
					continue
				}
				latencies[j.idx] = d
				if opts.OnQuestionRecall != nil {
					opts.OnQuestionRecall(q, artifacts)
				}
				if opts.OnQuestionRecallStageAudit != nil {
					opts.OnQuestionRecallStageAudit(q, audit)
				}
				var pred string
				var answerPrompt answerPromptRecord
				err = retryOnNotAvailable(qctx, "answer", q.ID, func() error {
					var aerr error
					pred, answerPrompt, aerr = buildPrediction(qctx, opts, q, artifacts, answerContext)
					return aerr
				})
				if err != nil {
					cancel()
					recordFailure(j.idx, q, "answer", err)
					maybeMilestone(done.Add(1))
					continue
				}
				em := boolFloat(metrics.ExactMatch(pred, q.GoldAnswers))
				f1 := metrics.F1(pred, q.GoldAnswers)
				var judge float64
				err = retryOnNotAvailable(qctx, "judge", q.ID, func() error {
					var jerr error
					judge, jerr = opts.Judge.Score(qctx, q.Query, pred, q.GoldAnswers)
					return jerr
				})
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
					khit := evidenceKHit(artifacts, q.EvidenceIDs)
					khitPtr = &khit
				}
				scores[j.idx] = QuestionScore{
					ID: q.ID, Query: q.Query, Prediction: pred,
					EM: em, F1: f1, Judge: judge, KHit: khitPtr,
					Tags: q.Tags,
				}
				if opts.OnQuestionAnswer != nil {
					opts.OnQuestionAnswer(NewAnswerReplayRecord(time.Now(), q, artifacts, AnswerReplayOutcome{
						Prediction: pred,
						EM:         em,
						F1:         f1,
						Judge:      judge,
						KHit:       khitPtr,
					}, answerPrompt.Template, answerPrompt.Body, answerPrompt.ContextFormat))
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

// turnBatch groups a contiguous slice of one conversation's turns by their
// SessionID. Both shapes (LLM messages + raw turns) are precomputed so the
// ingest loop can dispatch to either Runner variant without re-iterating.
type turnBatch struct {
	session        string
	msgs           []llm.Message
	rawTurns       []runners.RawTurn
	recentRawTurns []runners.RawTurn
}

const recentTurnsPerSavePoint = 12

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
			Parts: []model.Part{{Type: model.PartText, Text: renderDatasetTurnForMessage(t)}},
		})
		cur.rawTurns = append(cur.rawTurns, rawTurnFromDataset(t))
	}
	batches = append(batches, cur)
	return batches
}

// batchTurnsByOnlineSavePoint models online writes for extractor-backed runs.
// Each save point is a small adjacent exchange, rather than an isolated
// utterance, so question/answer and image-sharing context stays inside
// <extractable_evidence>. Previous turns from the same dataset session are passed as
// recent context, and future turns are never included.
func batchTurnsByOnlineSavePoint(c dataset.Conversation) []turnBatch {
	if len(c.Turns) == 0 {
		return nil
	}
	var batches []turnBatch
	recentBySession := map[string][]runners.RawTurn{}
	for i := 0; i < len(c.Turns); {
		session := c.Turns[i].SessionID
		end := i + 1
		for end < len(c.Turns) && c.Turns[end].SessionID == session && end-i < 2 {
			end++
		}

		raws := make([]runners.RawTurn, 0, end-i)
		msgs := make([]llm.Message, 0, end-i)
		for _, t := range c.Turns[i:end] {
			msgs = append(msgs, llm.Message{
				Role:  model.Role(t.Role),
				Parts: []model.Part{{Type: model.PartText, Text: renderDatasetTurnForMessage(t)}},
			})
			raws = append(raws, rawTurnFromDataset(t))
		}

		recent := recentBySession[session]
		if len(recent) > recentTurnsPerSavePoint {
			recent = recent[len(recent)-recentTurnsPerSavePoint:]
		}
		batches = append(batches, turnBatch{
			session:        session,
			msgs:           msgs,
			rawTurns:       raws,
			recentRawTurns: append([]runners.RawTurn(nil), recent...),
		})
		recentBySession[session] = append(recentBySession[session], raws...)
		i = end
	}
	return batches
}

func rawTurnFromDataset(t dataset.Turn) runners.RawTurn {
	return runners.RawTurn{
		Role:       t.Role,
		Content:    t.Content,
		Speaker:    t.Speaker,
		Timestamp:  t.Timestamp,
		EvidenceID: t.EvidenceID,
		SessionID:  t.SessionID,
		Images:     rawImagesFromDataset(t.Images),
	}
}

func renderDatasetTurnForMessage(t dataset.Turn) string {
	return runners.RenderRawTurnContent(rawTurnFromDataset(t))
}

func rawImagesFromDataset(images []dataset.Image) []runners.RawImage {
	if len(images) == 0 {
		return nil
	}
	out := make([]runners.RawImage, 0, len(images))
	for _, image := range images {
		raw := runners.RawImage{
			URL:     strings.TrimSpace(image.URL),
			Query:   strings.TrimSpace(image.Query),
			Caption: strings.TrimSpace(image.Caption),
		}
		if raw.URL == "" && raw.Query == "" && raw.Caption == "" {
			continue
		}
		out = append(out, raw)
	}
	return out
}
