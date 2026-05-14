// Package beir runs BEIR-style retrieval evaluations against
// sdk/knowledge. The original BEIR paper (https://arxiv.org/abs/2104.08663)
// and its 18-task leaderboard are the de-facto standard for measuring
// dense / sparse / hybrid retrievers on a common axis. We support the
// shape of any BEIR-format dataset (corpus.jsonl + queries.jsonl +
// qrels/test.tsv) so a regression we catch on SciFact today is reported
// in the same units the wider community publishes.
//
// We deliberately avoid bundling a "mini" copy of every BEIR task. The
// canonical datasets sit on HuggingFace under CC-BY-SA — see the cmd
// helper for how to download a single task with one curl line. Tests
// in this package fall back to a deterministic synthetic dataset so CI
// stays hermetic.
//
// Relation to eval/knowledge:
//   - eval/knowledge — our hand-curated 100-doc / 40-question Chinese
//     corpus; metrics are single-doc Recall@K + keyword coverage. Acts
//     as a deterministic PR gate.
//   - eval/beir       — public datasets with per-query *graded relevance*
//     judgments (qrels). Metrics are nDCG@k, Recall@k, MRR. Acts as a
//     comparable-to-leaderboard baseline.
//
// Both packages drive the same sdk/knowledge.Service; only the
// scoring layer differs.
package beir

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge/factory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Lane is an alias mirroring eval/knowledge so existing tooling
// (`--lanes bm25,vector,hybrid`) and downstream sinks stay uniform.
type Lane = knowledge.SearchMode

// Document is a single BEIR corpus entry. title and text are kept
// separate so we can mirror the original "title: \n text" formatting
// some BEIR baselines use; in practice we concatenate them when
// inserting into knowledge.Service because sdk/knowledge does not
// model titles distinctly today.
type Document struct {
	ID    string `json:"_id"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`
}

// Query is one row of queries.jsonl.
type Query struct {
	ID   string `json:"_id"`
	Text string `json:"text"`
}

// Qrel is one (query, doc, grade) triple from qrels/test.tsv.
// Grade follows BEIR convention: 0 = irrelevant, 1 = relevant,
// 2 = highly relevant. We treat anything > 0 as relevant for
// Recall@k and MRR; nDCG uses the graded form directly.
type Qrel struct {
	QueryID string
	DocID   string
	Grade   int
}

// Dataset bundles corpus + queries + qrels for a single BEIR task.
type Dataset struct {
	Name      string
	Documents []Document
	Queries   []Query
	Qrels     map[string]map[string]int // qID -> docID -> grade
}

// LoadDataset reads BEIR's canonical 3-file layout under root:
//
//	<root>/corpus.jsonl
//	<root>/queries.jsonl
//	<root>/qrels/test.tsv         (tab-separated: qID, docID, grade; first line is the header)
//
// Returns a Dataset whose Name is the basename of root.
func LoadDataset(root string) (*Dataset, error) {
	ds := &Dataset{
		Name:  filepath.Base(strings.TrimRight(root, string(filepath.Separator))),
		Qrels: map[string]map[string]int{},
	}

	if err := readJSONL(filepath.Join(root, "corpus.jsonl"), func(b []byte) error {
		var d Document
		if err := json.Unmarshal(b, &d); err != nil {
			return err
		}
		ds.Documents = append(ds.Documents, d)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read corpus: %w", err)
	}
	if err := readJSONL(filepath.Join(root, "queries.jsonl"), func(b []byte) error {
		var q Query
		if err := json.Unmarshal(b, &q); err != nil {
			return err
		}
		ds.Queries = append(ds.Queries, q)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read queries: %w", err)
	}
	if err := readQrels(filepath.Join(root, "qrels", "test.tsv"), ds.Qrels); err != nil {
		return nil, fmt.Errorf("read qrels: %w", err)
	}
	return ds, nil
}

func readJSONL(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if err := fn(raw); err != nil {
			return err
		}
	}
	return nil
}

func readQrels(path string, out map[string]map[string]int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i, line := range lines {
		// Skip the BEIR header row ("query-id\tcorpus-id\tscore") if
		// present; some redistributions omit it.
		if i == 0 && strings.HasPrefix(line, "query") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		grade, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil {
			continue
		}
		qID := strings.TrimSpace(fields[0])
		dID := strings.TrimSpace(fields[1])
		if out[qID] == nil {
			out[qID] = map[string]int{}
		}
		out[qID][dID] = grade
	}
	return nil
}

// DefaultCutoffs is the set of rank cutoffs every BEIR-style report
// emits. 10 is the BEIR paper's standard reporting depth; 100 is
// useful for pipelines that rerank deeper candidates.
var DefaultCutoffs = []int{10, 100}

// Options controls a Run.
type Options struct {
	// Embedder enables vector + hybrid lanes; nil restricts to BM25.
	Embedder knowledge.Embedder

	// DatasetID is the namespace inside the Service. Default: "beir".
	DatasetID string

	// Lanes selects which modes to score. Empty = bm25 + vector +
	// hybrid (vector / hybrid auto-skip when Embedder is nil).
	Lanes []Lane

	// Cutoffs are the rank depths reported as nDCG@k / Recall@k.
	// Default: [10, 100]. Must include the maximum cutoff used by the
	// Search call (we always request max(Cutoffs) hits).
	Cutoffs []int

	// IngestConcurrency caps simultaneous PutDocument calls. The
	// vector lane spends most of its wall-clock on embedding requests
	// during ingest, so this is the primary throughput knob.
	// Default: 8.
	IngestConcurrency int

	// QueryConcurrency caps in-flight searches. Default: 8.
	QueryConcurrency int

	// LimitQueries trims the query set for smoke runs. 0 = all.
	LimitQueries int

	// OverfetchFactor controls how many chunks we ask Search for per
	// unit of unique-doc cutoff before collapsing chunks-per-doc. Set
	// to 1 to disable over-fetch (top-K chunks only; rarely useful
	// once CollapseStrategy=sum is enabled because sum-pool needs
	// chunks beyond top-K to aggregate signal). Default: 4.
	OverfetchFactor int

	// CollapseStrategy selects how chunk scores are aggregated when
	// collapsing chunks→docID. The BEIR protocol expects doc-level
	// ranking (qrels are doc-level), so some aggregation is mandatory.
	//
	// scifact ablation (BM25 lane, 300 test queries):
	//
	//   strategy   overfetch   nDCG@10   recall@100   note
	//   max            4         0.180       0.255    default
	//   max            8         0.152       0.212    extra chunks dilute the best-chunk signal
	//   sum            4         0.054       0.207    length-biased on scifact
	//
	// Sum-pool was attempted on the textbook "re-aggregate multi-
	// keyword signal split across chunks" reasoning, but on scifact
	// it backfired: long non-relevant docs accumulate noise across
	// many chunks and outrank short relevant docs. Both numbers
	// remain far below the Lucene/Anserini doc-level BM25 baseline
	// (0.679) because the underlying retrieval path is chunk-level
	// by design; closing that gap requires a doc-level Search API
	// in sdk/knowledge (tracked in #126), not adapter-side tweaks.
	//
	//   - CollapseMax (default): keep highest-scoring chunk per
	//     docID. Most stable on length-skewed corpora.
	//
	//   - CollapseSum: sum chunk scores per docID. Kept for
	//     ablation; biased toward long docs on length-skewed
	//     corpora like scifact.
	//
	//   - CollapseFirst: keep first hit per docID in score-desc order
	//     (== max-pool for backends that return score-desc Hits, but
	//     does not require Hit.Score to be populated). Legacy.
	CollapseStrategy CollapseStrategy

	Hook        EventHook
	ProgressPct int
}

// CollapseStrategy names the chunks→docID aggregation function used
// by runLane. See Options.CollapseStrategy for the full rationale.
type CollapseStrategy string

const (
	CollapseSum   CollapseStrategy = "sum"
	CollapseMax   CollapseStrategy = "max"
	CollapseFirst CollapseStrategy = "first"
)

// DefaultOverfetchFactor is the chunk-overfetch multiplier applied
// before doc-collapse when Options.OverfetchFactor is zero.
const DefaultOverfetchFactor = 4

// DefaultCollapseStrategy is the aggregation function used when
// Options.CollapseStrategy is empty. Max-pool is the most stable
// choice on length-skewed corpora; see CollapseStrategy doc for the
// ablation data.
const DefaultCollapseStrategy = CollapseMax

// LaneReport's field names deliberately mirror eval/knowledge's
// LaneReport — same NDCG/Recall/MRR/LatencyP50/LatencyP95 layout —
// so anyone bouncing between the two suites' JSON output sees a
// consistent shape.
type LaneReport struct {
	Lane    Lane   `json:"lane"`
	Skipped string `json:"skipped,omitempty"`

	N        int `json:"n"` // queries scored (errors excluded)
	Errors   int `json:"errors"`
	NumQrels int `json:"num_qrels"` // total positive judgments observed

	// Aggregate metrics, indexed by cutoff.
	NDCG   map[int]float64 `json:"ndcg"`   // nDCG@k, graded
	Recall map[int]float64 `json:"recall"` // Recall@k, binary
	MRR    float64         `json:"mrr"`    // Mean reciprocal rank of the first relevant doc

	LatencyP50 time.Duration `json:"latency_p50"`
	LatencyP95 time.Duration `json:"latency_p95"`
}

// Report is the top-level JSON document the cmd writes.
type Report struct {
	Dataset    string               `json:"dataset"`
	StartedAt  time.Time            `json:"started_at"`
	DurationMS int64                `json:"duration_ms"`
	Options    map[string]any       `json:"options"`
	Lanes      map[Lane]*LaneReport `json:"lanes"`
}

// Event mirrors the canonical eval event shape.
type Event struct {
	Kind   string
	Time   time.Time
	Title  string
	Body   string
	Fields map[string]string
}

type EventHook func(ctx context.Context, e Event)

// Run scores opts.Lanes against ds and returns a Report. The corpus is
// ingested ONCE; each lane reuses the Service.
func Run(ctx context.Context, ds *Dataset, opts Options) (*Report, error) {
	if ds == nil {
		return nil, fmt.Errorf("beir: dataset is required")
	}
	if opts.DatasetID == "" {
		opts.DatasetID = "beir"
	}
	if len(opts.Cutoffs) == 0 {
		opts.Cutoffs = append([]int(nil), DefaultCutoffs...)
	}
	sort.Ints(opts.Cutoffs)
	if opts.IngestConcurrency <= 0 {
		opts.IngestConcurrency = 8
	}
	if opts.QueryConcurrency <= 0 {
		opts.QueryConcurrency = 8
	}
	if len(opts.Lanes) == 0 {
		opts.Lanes = []Lane{knowledge.ModeBM25, knowledge.ModeVector, knowledge.ModeHybrid}
	}
	if opts.OverfetchFactor <= 0 {
		opts.OverfetchFactor = DefaultOverfetchFactor
	}
	if opts.CollapseStrategy == "" {
		opts.CollapseStrategy = DefaultCollapseStrategy
	}

	// BEIR `queries.jsonl` carries all splits (train/dev/test). Only
	// queries present in `qrels/test.tsv` belong to the evaluation set;
	// the rest have hasIdeal=false and are silently dropped during
	// scoring. Filtering up-front avoids 3-4× wasted Search calls
	// (especially costly on the vector lane which also wastes
	// embedding-API quota).
	queries := make([]Query, 0, len(ds.Queries))
	for _, q := range ds.Queries {
		if len(ds.Qrels[q.ID]) > 0 {
			queries = append(queries, q)
		}
	}
	if opts.LimitQueries > 0 && len(queries) > opts.LimitQueries {
		queries = queries[:opts.LimitQueries]
	}

	rep := &Report{
		Dataset:   ds.Name,
		StartedAt: time.Now(),
		Lanes:     map[Lane]*LaneReport{},
		Options: map[string]any{
			"cutoffs":            opts.Cutoffs,
			"ingest_concurrency": opts.IngestConcurrency,
			"query_concurrency":  opts.QueryConcurrency,
			"has_embedder":       opts.Embedder != nil,
			"n_queries":          len(queries),
			// Chunk→doc collapse settings are stable per-run audit
			// trail for leaderboard.md (BEIR protocol requires
			// doc-level ranking; sdk/knowledge returns chunk-level).
			"collapse_to_doc":   true,
			"collapse_strategy": string(opts.CollapseStrategy),
			"overfetch_factor":  opts.OverfetchFactor,
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
		Body:  fmt.Sprintf("BEIR — %d docs / %d queries across %d lanes", len(ds.Documents), len(queries), len(opts.Lanes)),
		Fields: map[string]string{
			"dataset":   ds.Name,
			"n_docs":    fmt.Sprintf("%d", len(ds.Documents)),
			"n_queries": fmt.Sprintf("%d", len(queries)),
			"lanes":     strings.Join(laneNames, ","),
			"cutoffs":   fmt.Sprintf("%v", opts.Cutoffs),
		},
	})

	svc := buildService(opts)
	emit(Event{Kind: "ingest_start", Body: fmt.Sprintf("ingesting %d documents (concurrency=%d)", len(ds.Documents), opts.IngestConcurrency)})
	if err := ingest(ctx, svc, ds, opts, emit); err != nil {
		emit(Event{Kind: "error", Title: "ingest", Body: err.Error()})
		return nil, err
	}
	emit(Event{Kind: "ingest_done", Body: fmt.Sprintf("ingested %d documents", len(ds.Documents))})

	maxCutoff := opts.Cutoffs[len(opts.Cutoffs)-1]

	for _, lane := range opts.Lanes {
		emit(Event{Kind: "lane_start", Title: string(lane), Fields: map[string]string{"lane": string(lane)}})
		r := runLane(ctx, svc, ds, queries, lane, maxCutoff, opts, emit)
		rep.Lanes[lane] = r

		body := "SKIPPED: " + r.Skipped
		if r.Skipped == "" {
			body = fmt.Sprintf("nDCG@%d=%.3f recall@%d=%.3f mrr=%.3f errors=%d p95=%s",
				opts.Cutoffs[0], r.NDCG[opts.Cutoffs[0]],
				opts.Cutoffs[0], r.Recall[opts.Cutoffs[0]],
				r.MRR, r.Errors, r.LatencyP95.Round(time.Millisecond))
		}
		fields := map[string]string{
			"lane":   string(lane),
			"mrr":    fmt.Sprintf("%.3f", r.MRR),
			"errors": fmt.Sprintf("%d", r.Errors),
		}
		for _, k := range opts.Cutoffs {
			fields[fmt.Sprintf("ndcg_%d", k)] = fmt.Sprintf("%.3f", r.NDCG[k])
			fields[fmt.Sprintf("recall_%d", k)] = fmt.Sprintf("%.3f", r.Recall[k])
		}
		emit(Event{Kind: "lane_done", Title: string(lane), Body: body, Fields: fields})
	}

	doneParts := make([]string, 0, len(opts.Lanes))
	doneFields := map[string]string{"duration": time.Since(rep.StartedAt).Round(time.Second).String()}
	primaryK := opts.Cutoffs[0]
	for _, l := range opts.Lanes {
		if r := rep.Lanes[l]; r != nil && r.Skipped == "" {
			doneParts = append(doneParts, fmt.Sprintf("%s.nDCG@%d=%.3f", l, primaryK, r.NDCG[primaryK]))
			doneFields["ndcg_"+string(l)] = fmt.Sprintf("%.3f", r.NDCG[primaryK])
		}
	}
	emit(Event{Kind: "done", Title: ds.Name, Body: strings.Join(doneParts, " | "), Fields: doneFields})
	return rep, nil
}

func buildService(opts Options) *knowledge.Service {
	ws := workspace.NewMemWorkspace()
	var localOpts []factory.LocalOption
	if opts.Embedder != nil {
		localOpts = append(localOpts, factory.WithLocalEmbedder(opts.Embedder, opts.DatasetID))
	}
	return factory.NewLocal(ws, localOpts...)
}

// ingest puts every dataset document through PutDocument. BEIR corpora
// usually carry a title; we prefix it onto text with a blank line so
// the BM25 tokenizer sees both surfaces and dense models that pool
// title + text behave as their published baselines do.
//
// Emits an "ingest_progress" Event each time the cumulative doc count
// crosses an opts.ProgressPct milestone (default 10% steps). Ingest is
// typically the longest phase on bm25-only runs (entire corpus must be
// tokenized + reverse-indexed before any query can start), so silent
// ingest is the #1 driver of "is it stuck?" pages.
func ingest(ctx context.Context, svc *knowledge.Service, ds *Dataset, opts Options, emit func(Event)) error {
	sem := make(chan struct{}, opts.IngestConcurrency)
	var wg sync.WaitGroup
	var firstErr atomic.Value // error

	total := int64(len(ds.Documents))
	var milestones []int64
	if total > 0 && opts.ProgressPct > 0 && emit != nil {
		for pct := opts.ProgressPct; pct <= 99; pct += opts.ProgressPct {
			ms := total * int64(pct) / 100
			if ms < 1 {
				ms = 1
			}
			milestones = append(milestones, ms)
		}
	}
	var nextMs, done int64

	for _, d := range ds.Documents {
		d := d
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			body := d.Text
			if d.Title != "" {
				body = d.Title + "\n\n" + d.Text
			}
			// PutDocument is idempotent on (datasetID, name), so retries
			// during partial failures don't introduce duplicates.
			if err := svc.PutDocument(ctx, opts.DatasetID, d.ID, body); err != nil {
				firstErr.CompareAndSwap(nil, fmt.Errorf("put %s: %w", d.ID, err))
			}

			n := atomic.AddInt64(&done, 1)
			if len(milestones) == 0 {
				return
			}
			idx := atomic.LoadInt64(&nextMs)
			if idx >= int64(len(milestones)) || n < milestones[idx] {
				return
			}
			if !atomic.CompareAndSwapInt64(&nextMs, idx, idx+1) {
				return
			}
			pct := int64(opts.ProgressPct) * (idx + 1)
			emit(Event{
				Kind: "ingest_progress",
				Body: fmt.Sprintf("ingested %d/%d docs (~%d%%)", n, total, pct),
				Fields: map[string]string{
					"done":  fmt.Sprintf("%d", n),
					"total": fmt.Sprintf("%d", total),
					"pct":   fmt.Sprintf("%d", pct),
				},
			})
		}()
	}
	wg.Wait()
	if v := firstErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

func runLane(
	ctx context.Context,
	svc *knowledge.Service,
	ds *Dataset,
	queries []Query,
	lane Lane,
	topK int,
	opts Options,
	emit func(Event),
) *LaneReport {
	r := &LaneReport{
		Lane:   lane,
		NDCG:   map[int]float64{},
		Recall: map[int]float64{},
	}
	if (lane == knowledge.ModeVector || lane == knowledge.ModeHybrid) && opts.Embedder == nil {
		r.Skipped = "embedder not configured"
		return r
	}

	type result struct {
		err     bool
		latency time.Duration
		// rankedRelevance[i] is the BEIR grade (>0 = relevant) of the
		// document returned at rank i. Filled with 0 for hits whose
		// docID is not in qrels.
		rankedRelevance []int
		// hasIdeal indicates whether qrels carry at least one positive
		// for this query. Queries with no judgments are excluded from
		// the aggregate (consistent with BEIR's `evaluate` script).
		hasIdeal     bool
		idealRanking []int
	}
	results := make([]result, len(queries))

	sem := make(chan struct{}, opts.QueryConcurrency)
	var wg sync.WaitGroup
	var done int64
	total := len(queries)

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
	var nextMs int64

	for i, q := range queries {
		i := i
		q := q
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			out := result{}
			t0 := time.Now()
			// Over-fetch chunks because we collapse chunks→docID
			// below. Without this, top-K chunks can map to far
			// fewer than K unique docs (esp. for long-form
			// corpora), under-stating nDCG@K. Factor 4 covers
			// scifact's ~1-2 chunks/doc with margin; tune via
			// --overfetch / Options.OverfetchFactor (1 = disable
			// collapse, used only for ablation studies).
			fetchK := topK * opts.OverfetchFactor
			res, err := svc.Search(ctx, knowledge.Query{
				DatasetID: opts.DatasetID,
				Scope:     knowledge.ScopeSingleDataset,
				Text:      q.Text,
				Mode:      lane,
				TopK:      fetchK,
			})
			out.latency = time.Since(t0)
			if err != nil {
				out.err = true
				results[i] = out
				return
			}

			qrels := ds.Qrels[q.ID]
			ideal := make([]int, 0, len(qrels))
			for _, g := range qrels {
				if g > 0 {
					ideal = append(ideal, g)
				}
			}
			out.hasIdeal = len(ideal) > 0
			// Sort grades descending so DCG of an "ideal" ranking is
			// computable directly: the highest-graded relevant doc
			// would be at rank 1, the second-highest at rank 2, etc.
			sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
			out.idealRanking = ideal

			// Collapse chunks→docID using the configured strategy.
			// BEIR qrels are doc-level, so we MUST emit doc-level
			// ranking — the only question is how to aggregate the
			// chunk-level scores returned by Search.
			//
			// Sum-pool re-aggregates multi-keyword BM25 signal
			// distributed across a doc's chunks (the standard BEIR
			// adapter behaviour). Max-pool keeps the single best
			// chunk per doc; first-pool keeps the first hit per
			// doc in score-desc order (== max-pool for backends
			// that emit score-desc Hits but doesn't depend on
			// Hit.Score).
			//
			// All three sort docs by aggregated score (desc) and
			// truncate to topK AFTER aggregation. Doc grade is
			// looked up against qrels once per doc, regardless of
			// how many chunks contributed.
			ranked := collapseChunksToDocs(res.Hits, qrels, topK, opts.CollapseStrategy)
			out.rankedRelevance = ranked

			results[i] = out

			d := atomic.AddInt64(&done, 1)
			if len(milestones) > 0 {
				idx := atomic.LoadInt64(&nextMs)
				if idx < int64(len(milestones)) && d >= milestones[idx] {
					if atomic.CompareAndSwapInt64(&nextMs, idx, idx+1) {
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
	var sumNDCG = map[int]float64{}
	var sumRecall = map[int]float64{}
	var sumMRR float64
	var nScored int
	for _, s := range results {
		if s.err {
			r.Errors++
			continue
		}
		lats = append(lats, s.latency)
		if !s.hasIdeal {
			continue
		}
		nScored++
		r.NumQrels += len(s.idealRanking)
		for _, k := range opts.Cutoffs {
			sumNDCG[k] += nDCG(s.rankedRelevance, s.idealRanking, k)
			sumRecall[k] += recall(s.rankedRelevance, len(s.idealRanking), k)
		}
		sumMRR += mrr(s.rankedRelevance)
	}
	if nScored < 1 {
		nScored = 1
	}
	r.N = nScored
	r.MRR = sumMRR / float64(nScored)
	for _, k := range opts.Cutoffs {
		r.NDCG[k] = sumNDCG[k] / float64(nScored)
		r.Recall[k] = sumRecall[k] / float64(nScored)
	}
	r.LatencyP50, r.LatencyP95 = latencyPercentiles(lats)
	return r
}

// dcg returns the discounted cumulative gain of the first k grades.
// Uses the BEIR-standard exponential gain (2^grade - 1) which rewards
// graded relevance more than the linear variant.
func dcg(grades []int, k int) float64 {
	limit := k
	if len(grades) < limit {
		limit = len(grades)
	}
	var sum float64
	for i := 0; i < limit; i++ {
		gain := math.Pow(2, float64(grades[i])) - 1
		sum += gain / math.Log2(float64(i+2))
	}
	return sum
}

// nDCG computes nDCG@k normalising against the ideal ranking
// (descending list of all positive grades for the query).
// collapseChunksToDocs aggregates chunk-level Hits to a doc-level
// ranked list, applying the requested CollapseStrategy. The returned
// slice is len ≤ topK; each element is the qrels grade of the doc at
// that rank (0 = non-relevant or not-in-qrels).
//
// Sum-pool aggregates chunk scores per docID (re-aggregating signal
// that chunk-level BM25 splits across chunks of the same doc).
// Max-pool keeps the single highest-scoring chunk per docID.
// First-pool keeps the first hit per docID in input order (== max-pool
// when the caller's Hits are score-desc sorted; doesn't depend on
// Hit.Score being populated).
//
// All three sort by aggregated score (desc), with docID lexicographic
// ascending as a deterministic tie-breaker so two scorers with
// identical chunk-score distributions produce identical rankings
// across re-runs.
func collapseChunksToDocs(hits []knowledge.Hit, qrels map[string]int, topK int, strategy CollapseStrategy) []int {
	if topK <= 0 {
		return nil
	}
	type docAccum struct {
		score float64
		grade int
		first int // input-order index of first hit, for CollapseFirst
		idx   int // input-order index for tie-breaking
	}
	docs := make(map[string]*docAccum, topK)
	order := make([]string, 0, topK)
	for i, h := range hits {
		if h.DocName == "" {
			continue
		}
		d, ok := docs[h.DocName]
		if !ok {
			d = &docAccum{grade: qrels[h.DocName], first: i, idx: i}
			docs[h.DocName] = d
			order = append(order, h.DocName)
		}
		switch strategy {
		case CollapseSum:
			d.score += h.Score
		case CollapseMax:
			if h.Score > d.score || !ok {
				d.score = h.Score
			}
		case CollapseFirst:
			if !ok {
				d.score = h.Score
			}
		default:
			// Unknown strategy → behave as sum (defensive default).
			d.score += h.Score
		}
	}

	type rankedDoc struct {
		name  string
		score float64
		grade int
		idx   int
	}
	out := make([]rankedDoc, 0, len(docs))
	for _, name := range order {
		d := docs[name]
		out = append(out, rankedDoc{name: name, score: d.score, grade: d.grade, idx: d.idx})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		// Stable, deterministic tie-break: earliest-seen docID wins.
		// This makes re-runs against the same backend bit-identical.
		return out[i].idx < out[j].idx
	})
	if len(out) > topK {
		out = out[:topK]
	}
	ranked := make([]int, 0, len(out))
	for _, d := range out {
		ranked = append(ranked, d.grade)
	}
	return ranked
}

func nDCG(ranked, ideal []int, k int) float64 {
	idealDCG := dcg(ideal, k)
	if idealDCG == 0 {
		return 0
	}
	return dcg(ranked, k) / idealDCG
}

// recall counts how many of the totalRel relevant documents (any grade
// > 0) appear inside the top-k ranking.
func recall(ranked []int, totalRel, k int) float64 {
	if totalRel == 0 {
		return 0
	}
	limit := k
	if len(ranked) < limit {
		limit = len(ranked)
	}
	var hits int
	for i := 0; i < limit; i++ {
		if ranked[i] > 0 {
			hits++
		}
	}
	return float64(hits) / float64(totalRel)
}

// mrr returns 1/(rank of first relevant doc) or 0 if none in the list.
func mrr(ranked []int) float64 {
	for i, g := range ranked {
		if g > 0 {
			return 1.0 / float64(i+1)
		}
	}
	return 0
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
