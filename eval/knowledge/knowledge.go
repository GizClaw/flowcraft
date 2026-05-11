// Package knowledgequality is the retrieval-quality regression suite for
// sdk/knowledge. The Run function spins up an in-memory Service, ingests
// a corpus, replays a golden question set across one or more search
// lanes (bm25 / vector / hybrid) and returns a structured Report.
//
// Two entry points share the same Run engine:
//
//   - eval/knowledge/cmd/eval — standalone binary that writes a JSON
//     Report, suitable for CI / nightly comparisons.
//   - the *_test.go files in this package — call Run for each lane and
//     assert recall / keyword / negative thresholds.
//
// The split mirrors eval/locomo: tests gate merges, the binary fuels
// long-run trend dashboards.
package knowledgequality

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge/factory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Lane is an alias so callers don't have to import sdk/knowledge twice
// to write `[]Lane{knowledge.ModeBM25, knowledge.ModeVector}`.
type Lane = knowledge.SearchMode

// Question mirrors a single line of golden.jsonl. Category "negative"
// rows must NOT surface their (absent) ExpectedDoc; all other rows must
// surface it inside the top-K hits.
type Question struct {
	ID               string   `json:"id"`
	Category         string   `json:"category"`
	Question         string   `json:"question"`
	ExpectedDoc      string   `json:"expected_doc"`
	ExpectedKeywords []string `json:"expected_keywords"`
}

// Dataset packages a document corpus with its golden question set so the
// caller can hand it around as a single value.
type Dataset struct {
	Name      string
	Documents map[string]string // DocName → body (markdown). Keys are doc filenames.
	Questions []Question
}

// LoadDatasetFromDir reads testdata-style trees: every `*.md` under
// corpusDir becomes a document keyed by filename; goldenPath is the
// JSONL question set.
//
// Empty / whitespace lines and `#`-prefixed comments are skipped. Lines
// keep their disk order so a failing question can be located by line
// number in the source file.
func LoadDatasetFromDir(corpusDir, goldenPath string) (*Dataset, error) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		return nil, fmt.Errorf("read corpus dir %q: %w", corpusDir, err)
	}
	docs := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(corpusDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		docs[e.Name()] = string(body)
	}

	f, err := os.Open(goldenPath)
	if err != nil {
		return nil, fmt.Errorf("open golden %q: %w", goldenPath, err)
	}
	defer f.Close()

	var qs []Question
	sc := bufio.NewScanner(f)
	// Scanner default buffer is 64 KiB; bump to 1 MiB so unusually long
	// questions (e.g. paragraph-style queries) don't trigger
	// bufio.ErrTooLong on otherwise-valid golden files.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var q Question
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			return nil, fmt.Errorf("parse golden line %q: %w", line, err)
		}
		qs = append(qs, q)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan golden: %w", err)
	}

	return &Dataset{
		Name:      filepath.Base(corpusDir),
		Documents: docs,
		Questions: qs,
	}, nil
}

// DefaultTopK is the rank cutoff applied when [Options.TopK] is zero.
// It matches the historical knowledge integration suite so absolute
// recall numbers stay comparable across the migration.
const DefaultTopK = 5

// Options controls a Run.
type Options struct {
	// Embedder enables the vector + hybrid lanes; nil restricts the
	// run to BM25 only (any vector/hybrid lane in Lanes is then marked
	// Skipped, not erred, so a credential-less CI lane still produces
	// a complete Report).
	Embedder knowledge.Embedder

	// DatasetID is the namespace inside the Service. Tests share this
	// across lanes so the corpus is ingested once per Service.
	// Default: "e2e".
	DatasetID string

	// Lanes selects which search modes to score. Empty = all three
	// (BM25 always runs; Vector/Hybrid skip when Embedder is nil).
	Lanes []Lane

	// TopK is the rank cutoff used for Recall@K and the keyword
	// content check. Default: [DefaultTopK].
	TopK int

	// Concurrency caps in-flight searches per lane. Vector lanes hit a
	// real embedding endpoint per query so leaving this high is the
	// usual reason a Run finishes in seconds rather than minutes.
	// Default: 4.
	Concurrency int

	// NegativeScoreCeiling, when > 0, marks any negative-class query
	// whose top-1 score exceeds this threshold as a breach (counted
	// in LaneReport.NegativeBreach). Use 0 to disable.
	NegativeScoreCeiling float64

	// MaxMissSamples caps how many missed positives we store on
	// LaneReport.Misses (for human debugging). Default: 10.
	MaxMissSamples int

	// Hook receives lifecycle events when non-nil. CLI wires this to a
	// Feishu notifier; tests typically leave it nil.
	Hook EventHook

	// ProgressPct gates per-lane progress events: a milestone fires
	// every ProgressPct percent of completed questions. <=0 disables
	// intermediate progress (start / lane_done / done still fire).
	ProgressPct int
}

// MissSample records a positive-class question whose ExpectedDoc was
// not in the top-K hits. Stored on LaneReport so callers can log
// human-readable diagnostics without re-running.
type MissSample struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Expected string   `json:"expected"`
	TopK     []string `json:"top_k"` // doc names, in returned order
}

// KeywordShortfall records a hit that landed the expected doc but
// whose content does not contain every required keyword. Indicates the
// document was retrieved but the relevant chunk was not.
type KeywordShortfall struct {
	ID       string   `json:"id"`
	Expected string   `json:"expected"`
	Missing  []string `json:"missing"`
}

// LaneReport is the per-lane scoring summary.
type LaneReport struct {
	Lane    Lane   `json:"lane"`
	Skipped string `json:"skipped,omitempty"` // non-empty = lane was not scored (e.g. embedder missing for vector)

	N         int `json:"n"`         // total scored questions
	Positives int `json:"positives"` // category != "negative"
	Negatives int `json:"negatives"` // category == "negative"
	Errors    int `json:"errors"`    // search calls that returned an error

	RecallHits     int     `json:"recall_hits"`     // positives whose ExpectedDoc appeared in top-K
	RecallAtK      float64 `json:"recall_at_k"`     // RecallHits / Positives
	KeywordHits    int     `json:"keyword_hits"`    // among RecallHits, content contained all keywords
	KeywordRate    float64 `json:"keyword_rate"`    // KeywordHits / RecallHits
	NegativeBreach int     `json:"negative_breach"` // negatives whose top-1 score > Options.NegativeScoreCeiling

	LatencyP50 time.Duration `json:"latency_p50"`
	LatencyP95 time.Duration `json:"latency_p95"`

	Misses             []MissSample       `json:"misses,omitempty"`
	KeywordShortfalls  []KeywordShortfall `json:"keyword_shortfalls,omitempty"`
	NegativeBreachIDs  []string           `json:"negative_breach_ids,omitempty"`
}

// Report is the top-level JSON document the cmd writes.
type Report struct {
	Dataset    string                `json:"dataset"`
	StartedAt  time.Time             `json:"started_at"`
	DurationMS int64                 `json:"duration_ms"`
	Options    map[string]any        `json:"options"`
	Lanes      map[Lane]*LaneReport  `json:"lanes"`
}

// Event mirrors the locomo/history event shape so a single notify
// adapter can forward all three eval suites.
type Event struct {
	Kind   string // start | lane_start | lane_progress | lane_done | done | error
	Time   time.Time
	Title  string
	Body   string
	Fields map[string]string
}

// EventHook receives lifecycle events. Must be non-blocking; Run
// invokes it on the per-lane goroutine.
type EventHook func(ctx context.Context, e Event)

// defaultLanes returns the standard scoring order when Options.Lanes is
// empty. BM25 always runs (no creds); Vector/Hybrid are appended in
// dependency order so a partial credential set still produces a
// well-shaped report.
func defaultLanes() []Lane {
	return []Lane{knowledge.ModeBM25, knowledge.ModeVector, knowledge.ModeHybrid}
}

// Run executes opts.Lanes against ds and returns a Report. The corpus
// is ingested ONCE; each lane reuses the same Service, so the
// hybrid-vs-bm25 comparison sees identical state.
func Run(ctx context.Context, ds *Dataset, opts Options) (*Report, error) {
	if ds == nil {
		return nil, fmt.Errorf("knowledge: dataset is required")
	}
	if opts.DatasetID == "" {
		opts.DatasetID = "e2e"
	}
	if opts.TopK <= 0 {
		opts.TopK = DefaultTopK
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.MaxMissSamples <= 0 {
		opts.MaxMissSamples = 10
	}
	if len(opts.Lanes) == 0 {
		opts.Lanes = defaultLanes()
	}

	rep := &Report{
		Dataset:   ds.Name,
		StartedAt: time.Now(),
		Lanes:     map[Lane]*LaneReport{},
		Options: map[string]any{
			"topk":         opts.TopK,
			"concurrency":  opts.Concurrency,
			"neg_ceiling":  opts.NegativeScoreCeiling,
			"has_embedder": opts.Embedder != nil,
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

	laneNames := make([]string, 0, len(opts.Lanes))
	for _, l := range opts.Lanes {
		laneNames = append(laneNames, string(l))
	}
	emit(Event{
		Kind:  "start",
		Title: ds.Name,
		Body:  fmt.Sprintf("knowledge — %d docs / %d qs across %d lanes", len(ds.Documents), len(ds.Questions), len(opts.Lanes)),
		Fields: map[string]string{
			"dataset": ds.Name,
			"n_docs":  fmt.Sprintf("%d", len(ds.Documents)),
			"n_qs":    fmt.Sprintf("%d", len(ds.Questions)),
			"lanes":   strings.Join(laneNames, ","),
			"topk":    fmt.Sprintf("%d", opts.TopK),
		},
	})

	// Build + ingest once; reused across lanes.
	svc, err := buildService(opts)
	if err != nil {
		emit(Event{Kind: "error", Title: "service build", Body: err.Error()})
		return nil, err
	}
	if err := ingest(ctx, svc, ds, opts.DatasetID); err != nil {
		emit(Event{Kind: "error", Title: "ingest", Body: err.Error()})
		return nil, err
	}

	for _, lane := range opts.Lanes {
		emit(Event{
			Kind:   "lane_start",
			Title:  string(lane),
			Body:   fmt.Sprintf("lane %s starting", lane),
			Fields: map[string]string{"lane": string(lane)},
		})
		r := runLane(ctx, svc, ds, lane, opts, emit)
		rep.Lanes[lane] = r

		body := fmt.Sprintf("recall@%d=%.3f keyword=%.3f neg_breach=%d errors=%d p95=%s",
			opts.TopK, r.RecallAtK, r.KeywordRate, r.NegativeBreach, r.Errors, r.LatencyP95.Round(time.Millisecond))
		if r.Skipped != "" {
			body = "SKIPPED: " + r.Skipped
		}
		emit(Event{
			Kind:  "lane_done",
			Title: string(lane),
			Body:  body,
			Fields: map[string]string{
				"lane":            string(lane),
				"recall_at_k":     fmt.Sprintf("%.3f", r.RecallAtK),
				"keyword_rate":    fmt.Sprintf("%.3f", r.KeywordRate),
				"negative_breach": fmt.Sprintf("%d", r.NegativeBreach),
				"errors":          fmt.Sprintf("%d", r.Errors),
			},
		})
	}

	doneParts := make([]string, 0, len(opts.Lanes))
	doneFields := map[string]string{"duration": time.Since(rep.StartedAt).Round(time.Second).String()}
	for _, l := range opts.Lanes {
		if r := rep.Lanes[l]; r != nil && r.Skipped == "" {
			doneParts = append(doneParts, fmt.Sprintf("%s=%.3f", l, r.RecallAtK))
			doneFields["recall_"+string(l)] = fmt.Sprintf("%.3f", r.RecallAtK)
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

// buildService spins up a fresh in-memory workspace + Service. The
// embedder is wired only when non-nil — a nil embedder produces a
// BM25-only Service that still answers ModeBM25 queries correctly but
// returns empty results for ModeVector / ModeHybrid (we surface those
// as Skipped lanes upstream rather than crashing).
func buildService(opts Options) (*knowledge.Service, error) {
	ws := workspace.NewMemWorkspace()
	var localOpts []factory.LocalOption
	if opts.Embedder != nil {
		localOpts = append(localOpts, factory.WithLocalEmbedder(opts.Embedder, opts.DatasetID))
	}
	return factory.NewLocal(ws, localOpts...), nil
}

// ingest puts every dataset document into svc under datasetID.
func ingest(ctx context.Context, svc *knowledge.Service, ds *Dataset, datasetID string) error {
	for name, body := range ds.Documents {
		if err := svc.PutDocument(ctx, datasetID, name, body); err != nil {
			return fmt.Errorf("put %s: %w", name, err)
		}
	}
	return nil
}

// runLane scores ds.Questions under one search mode and returns the
// LaneReport. The function is intentionally side-effect-free apart
// from the emit callback so two lanes can be parallelised by the
// caller in the future without coordination beyond the embedder rate
// limit (which we already cap via opts.Concurrency).
func runLane(ctx context.Context, svc *knowledge.Service, ds *Dataset, lane Lane, opts Options, emit func(Event)) *LaneReport {
	r := &LaneReport{Lane: lane}

	if (lane == knowledge.ModeVector || lane == knowledge.ModeHybrid) && opts.Embedder == nil {
		r.Skipped = "embedder not configured"
		return r
	}

	type sample struct {
		err          bool
		latency      time.Duration
		recallHit    bool
		keywordHit   bool
		keywordMiss  *KeywordShortfall
		miss         *MissSample
		negative     bool
		negBreached  bool
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

	// Pre-compute progress milestones once so the hot path is a single
	// atomic load + compare. Mirrors the cheap fast-path used in
	// eval/history/eval.go.
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
	var nextMilestoneIdx int64

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
			res, err := svc.Search(ctx, knowledge.Query{
				DatasetID: opts.DatasetID,
				Scope:     knowledge.ScopeSingleDataset,
				Text:      q.Question,
				Mode:      lane,
				TopK:      opts.TopK,
			})
			out.latency = time.Since(t0)
			if err != nil {
				out.err = true
				results[i] = out
				return
			}

			if q.Category == "negative" {
				out.negative = true
				if opts.NegativeScoreCeiling > 0 && len(res.Hits) > 0 && res.Hits[0].Score > opts.NegativeScoreCeiling {
					out.negBreached = true
				}
				results[i] = out
			} else {
				topNames := make([]string, 0, len(res.Hits))
				for _, h := range res.Hits {
					topNames = append(topNames, h.DocName)
				}
				for _, h := range res.Hits {
					if h.DocName == q.ExpectedDoc {
						out.recallHit = true
						missing := missingKeywords(h.Content, q.ExpectedKeywords)
						if len(missing) == 0 {
							out.keywordHit = true
						} else {
							out.keywordMiss = &KeywordShortfall{ID: q.ID, Expected: q.ExpectedDoc, Missing: missing}
						}
						break
					}
				}
				if !out.recallHit {
					out.miss = &MissSample{ID: q.ID, Question: q.Question, Expected: q.ExpectedDoc, TopK: topNames}
				}
				results[i] = out
			}

			d := atomic.AddInt64(&done, 1)
			if len(milestones) > 0 {
				idx := atomic.LoadInt64(&nextMilestoneIdx)
				if idx < int64(len(milestones)) && d >= milestones[idx] {
					if atomic.CompareAndSwapInt64(&nextMilestoneIdx, idx, idx+1) {
						pct := int64(opts.ProgressPct) * (idx + 1)
						emit(Event{
							Kind: "lane_progress",
							Body: fmt.Sprintf("%s %d/%d (~%d%%)", lane, d, total, pct),
							Fields: map[string]string{
								"lane":  string(lane),
								"done":  fmt.Sprintf("%d", d),
								"total": fmt.Sprintf("%d", total),
								"pct":   fmt.Sprintf("%d", pct),
							},
						})
					}
				}
			}
		}()
	}
	wg.Wait()

	lats := make([]time.Duration, 0, total)
	for _, s := range results {
		if s.err {
			r.Errors++
			continue
		}
		lats = append(lats, s.latency)
		if s.negative {
			r.Negatives++
			if s.negBreached {
				r.NegativeBreach++
				// We can't recover the ID without re-indexing; the
				// caller-facing list is built on the second pass below.
			}
			continue
		}
		r.Positives++
		if s.recallHit {
			r.RecallHits++
			if s.keywordHit {
				r.KeywordHits++
			} else if s.keywordMiss != nil && len(r.KeywordShortfalls) < opts.MaxMissSamples {
				r.KeywordShortfalls = append(r.KeywordShortfalls, *s.keywordMiss)
			}
		} else if s.miss != nil && len(r.Misses) < opts.MaxMissSamples {
			r.Misses = append(r.Misses, *s.miss)
		}
	}
	for i, s := range results {
		if s.negBreached {
			r.NegativeBreachIDs = append(r.NegativeBreachIDs, ds.Questions[i].ID)
		}
	}

	r.N = r.Positives + r.Negatives
	r.RecallAtK = ratio(r.RecallHits, r.Positives)
	r.KeywordRate = ratio(r.KeywordHits, r.RecallHits)
	r.LatencyP50, r.LatencyP95 = latencyPercentiles(lats)
	return r
}

func missingKeywords(content string, kws []string) []string {
	var missing []string
	for _, kw := range kws {
		if !strings.Contains(content, kw) {
			missing = append(missing, kw)
		}
	}
	return missing
}

func ratio(n, d int) float64 {
	if d == 0 {
		// Conventional value when there is nothing to score: a metric
		// "no failed questions" reads as 1.0, not 0.0. Mirrors the
		// historical helper_test.go behaviour.
		return 1
	}
	return float64(n) / float64(d)
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
