package recall

import (
	"context"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Request is the input to Memory.Recall ( + §6.2).
type Request struct {
	Query  string
	TopK   int
	Filter map[string]any // metadata equality filters merged into pipeline filter
	Now    time.Time      // optional clock injection (for tests / TTL)

	// WithStale, if true, disables the default ExpireFilter so expired
	// entries are returned alongside live ones.
	WithStale bool

	// WithTombstoned, if true, disables the default TombstoneFilter so
	// entries the LLM update resolver marked with metadata.tombstone =
	// true are returned alongside live ones. Use this to debug
	// resolver behaviour or to re-surface entries before
	// Auditable.Rollback. Note that MetaTombstone is a RESERVED
	// metadata key written by recall internals — pre-existing user
	// data accidentally stored under "tombstone" will be hidden by
	// default until WithTombstoned is set.
	WithTombstoned bool

	// Debug controls how much execution detail the underlying retrieval
	// pipeline should attach. Memory.Recall always discards it; callers
	// that need the SearchExecution must use [RecallExplainer.RecallExplain].
	Debug retrieval.SearchDebug
}

// Hit is one result returned by Memory.Recall.
type Hit struct {
	Entry  Entry
	Score  float64
	Scores map[string]float64
}

// Recall runs the configured pipeline against the namespace and projects
// hits back into Entry.
//
// Telemetry: emits span memory.recall.recall covering the entire
// pipeline.Run call (which itself emits per-stage child spans), records
// outcome on counter memory.recall.recall_total, latency on histogram
// memory.recall.recall_duration, and hit count on histogram
// memory.recall.recall_hits. Query text is intentionally NOT attached
// to the span (PII risk on user-typed queries).
//
// Memory.Recall does NOT expose the underlying [retrieval.SearchExecution];
// callers that need it must type-assert to [RecallExplainer] and call
// RecallExplain instead.
func (m *lt) Recall(ctx context.Context, scope Scope, req Request) ([]Hit, error) {
	hits, _, err := m.runRecall(ctx, scope, req)
	return hits, err
}

// RecallExplain is the structured-explanation variant of Recall: it returns
// the same hits plus the underlying [retrieval.SearchExecution] (lanes,
// stages) when Request.Debug requested it. Execution is nil when Debug is
// zero or when no stage chose to populate it.
//
// Implements [RecallExplainer].
func (m *lt) RecallExplain(ctx context.Context, scope Scope, req Request) ([]Hit, *retrieval.SearchExecution, error) {
	return m.runRecall(ctx, scope, req)
}

// runRecall is the single internal recall path; both [Memory.Recall] and
// [RecallExplainer.RecallExplain] delegate here so telemetry, scope
// validation, namespace bookkeeping and pipeline wiring stay in one place.
//
// The returned *retrieval.SearchExecution is whatever the configured
// pipeline produced; callers that don't want it discard it (Memory.Recall).
func (m *lt) runRecall(ctx context.Context, scope Scope, req Request) ([]Hit, *retrieval.SearchExecution, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "memory.recall.recall")
	defer span.End()
	t0 := time.Now()
	defer func() {
		recallDuration.Record(ctx, time.Since(t0).Seconds())
	}()

	if err := m.validateScope(scope); err != nil {
		span.RecordError(err)
		recallTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "fail"), attribute.String("reason", "scope")))
		return nil, nil, err
	}
	now := req.Now
	if now.IsZero() {
		now = m.cfg.now()
	}
	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}
	filter := AgentRecallFilter(scope)
	if !req.WithStale {
		filter = MergeFilters(filter, ExpireFilter(now))
	}
	// Tombstones (set by the LLM update resolver's DELETE action) are
	// hidden from Recall by default. Callers can opt out via
	// Request.WithTombstoned to debug resolver behaviour or to
	// re-surface entries before Auditable.Rollback. Forget remains
	// the way to hard-delete them.
	if !req.WithTombstoned {
		filter = MergeFilters(filter, TombstoneFilter())
	}
	if len(req.Filter) > 0 {
		filter = MergeFilters(filter, retrieval.Filter{Eq: req.Filter})
	}
	// Multi-partition recall (#150): each effective partition maps
	// to its own namespace; the pipeline runs per-namespace and the
	// per-partition results are merged client-side (dedup by
	// Doc.ID with max-score retention, sort by score desc, truncate
	// to TopK). Single-partition recall is the hot path and is
	// returned verbatim — no merge overhead, full Execution
	// preserved.
	nss := namespacesForRecall(scope)
	for _, ns := range nss {
		m.rememberNamespace(ctx, ns)
	}
	span.SetAttributes(
		attribute.String("runtime_id", scope.RuntimeID),
		attribute.Bool("has_user_id", scope.UserID != ""),
		attribute.Int("top_k", topK),
		attribute.Int("query_len", len(req.Query)),
		attribute.Bool("with_stale", req.WithStale),
		attribute.Bool("with_tombstoned", req.WithTombstoned),
		attribute.Bool("debug_lanes", req.Debug.IncludeLanes),
		attribute.Bool("debug_stages", req.Debug.IncludeStages),
		attribute.Int("partitions", len(nss)),
	)
	// Always ask the pipeline for stage + lane trace so we can feed
	// recallStageDuration / recallLaneDuration without depending on
	// per-call SearchDebug. The extra cost is bounded: stages are tiny
	// structs; lanes carry copied hit slices but recall TopK is in the
	// dozens. The caller-visible Execution is trimmed below to match
	// req.Debug so RecallExplain still honours its public contract
	// ("Execution is nil when Debug is zero").
	pipeDebug := req.Debug
	pipeDebug.IncludeStages = true
	pipeDebug.IncludeLanes = true
	searchReq := retrieval.SearchRequest{
		QueryText: req.Query,
		Filter:    filter,
		TopK:      topK,
		Debug:     pipeDebug,
	}
	var (
		allHits   []retrieval.Hit
		firstExec *retrieval.SearchExecution
	)
	for _, ns := range nss {
		resp, err := m.pipe.Run(ctx, m.idx, ns, searchReq)
		if err != nil {
			span.RecordError(err)
			recallTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "fail"), attribute.String("reason", "pipeline")))
			return nil, nil, err
		}
		recordStageDurations(ctx, resp.Execution)
		recordLaneDurations(ctx, resp.Execution)
		if firstExec == nil {
			firstExec = resp.Execution
		}
		allHits = append(allHits, resp.Hits...)
	}
	// Merge across partitions when more than one was visited. The
	// single-partition path is the common case and stays verbatim
	// (no merge sort, identical hit ordering).
	merged := allHits
	if len(nss) > 1 {
		merged = mergePartitionHits(allHits, topK)
	}
	out := make([]Hit, 0, len(merged))
	for _, h := range merged {
		out = append(out, Hit{
			Entry:  DocToEntry(h.Doc),
			Score:  h.Score,
			Scores: h.Scores,
		})
	}
	span.SetAttributes(attribute.Int("hits", len(out)))
	recallHits.Record(ctx, int64(len(out)))
	recallTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "success")))
	return out, projectExecution(firstExec, req.Debug), nil
}

// mergePartitionHits deduplicates per-doc hits across multiple
// partitions, keeping the highest-scored copy of each Doc.ID,
// orders by score descending (Doc.ID asc as a deterministic tie
// breaker), and truncates to topK. Empty inputs return nil to
// match the single-partition behaviour.
//
// This is the merge half of the [namespacesForRecall] fan-out: each
// partition runs its own LTM pipeline (per-namespace SupersededDecay
// / SlotCollapse / TimeDecay / EntityBoost), so cross-partition
// soft-ranking signals stay correct; only the final per-doc winner
// is picked here.
func mergePartitionHits(hits []retrieval.Hit, topK int) []retrieval.Hit {
	if len(hits) == 0 {
		return nil
	}
	best := make(map[string]int, len(hits))
	for i, h := range hits {
		prevIdx, ok := best[h.Doc.ID]
		if !ok || h.Score > hits[prevIdx].Score {
			best[h.Doc.ID] = i
		}
	}
	out := make([]retrieval.Hit, 0, len(best))
	for _, idx := range best {
		out = append(out, hits[idx])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Doc.ID < out[j].Doc.ID
	})
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out
}

// projectExecution returns the caller-visible slice of pipe.Execution
// according to req.Debug. Pipeline always emits stages (we need them for
// recallStageDuration), but callers must explicitly request them via
// SearchDebug.IncludeStages to observe them; otherwise we strip stages
// (and lanes if not requested) so RecallExplain's public contract holds.
func projectExecution(exec *retrieval.SearchExecution, debug retrieval.SearchDebug) *retrieval.SearchExecution {
	if exec == nil {
		return nil
	}
	if !debug.IncludeLanes && !debug.IncludeStages {
		return nil
	}
	out := &retrieval.SearchExecution{}
	if debug.IncludeLanes {
		out.Lanes = exec.Lanes
	}
	if debug.IncludeStages {
		out.Stages = exec.Stages
	}
	return out
}

// recordStageDurations emits one observation per pipeline stage on
// recallStageDuration. The stage label is the pipeline stage name; the
// outcome label is "success" by default and "fail" when the stage
// recorded an error (pipeline aborts on first error, so at most one
// "fail" sample is produced per call).
func recordStageDurations(ctx context.Context, exec *retrieval.SearchExecution) {
	if exec == nil || len(exec.Stages) == 0 {
		return
	}
	for _, st := range exec.Stages {
		outcome := "success"
		if st.Err != "" {
			outcome = "fail"
		}
		recallStageDuration.Record(
			ctx,
			st.Took.Seconds(),
			metric.WithAttributes(
				attribute.String("stage", st.Name),
				attribute.String("outcome", outcome),
			),
		)
	}
}

// recordLaneDurations emits one observation per recall lane on
// recallLaneDuration. The lane label is the LaneKey reported by the
// pipeline (a small enum-like set, see retrieval.Lane*). Lanes whose
// Took is zero are skipped — typically a lane that the recall stage
// never invoked (e.g. vector lane when QueryVector is empty).
func recordLaneDurations(ctx context.Context, exec *retrieval.SearchExecution) {
	if exec == nil || len(exec.Lanes) == 0 {
		return
	}
	for _, lane := range exec.Lanes {
		if lane.Took <= 0 {
			continue
		}
		recallLaneDuration.Record(
			ctx,
			lane.Took.Seconds(),
			metric.WithAttributes(
				attribute.String("lane", string(lane.Key)),
			),
		)
	}
}

// History implements Memory; requires Journal.
func (m *lt) History(ctx context.Context, scope Scope, id string) ([]journal.Event, error) {
	if m.cfg.journal == nil {
		return nil, ErrJournalRequired
	}
	if err := m.validateScope(scope); err != nil {
		return nil, err
	}
	ns := NamespaceFor(scope)
	m.rememberNamespace(ctx, ns)
	return m.cfg.journal.History(ctx, ns, id)
}

// Rollback re-applies the last Upsert recorded before t (or deletes the doc
// when no prior state existed).
func (m *lt) Rollback(ctx context.Context, scope Scope, id string, before time.Time) error {
	if m.cfg.journal == nil {
		return ErrJournalRequired
	}
	if err := m.validateScope(scope); err != nil {
		return err
	}
	ns := NamespaceFor(scope)
	m.rememberNamespace(ctx, ns)
	events, err := m.cfg.journal.History(ctx, ns, id)
	if err != nil {
		return err
	}
	var target *retrieval.Doc
	for _, e := range events {
		if e.Timestamp.After(before) {
			break
		}
		switch e.Op {
		case journal.OpUpsert:
			if e.After != nil {
				cp := *e.After
				target = &cp
			}
		case journal.OpDelete:
			target = nil
		}
	}
	if target == nil {
		return m.idx.Delete(ctx, ns, []string{id})
	}
	return m.idx.Upsert(ctx, ns, []retrieval.Doc{*target})
}

// Forget hard-deletes one entry; Journal records OpDelete{reason}.
func (m *lt) Forget(ctx context.Context, scope Scope, id, reason string) error {
	_ = reason // reason is captured by Journal actor (caller can WithActor)
	if err := m.validateScope(scope); err != nil {
		return err
	}
	ns := NamespaceFor(scope)
	m.rememberNamespace(ctx, ns)
	if err := m.idx.Delete(ctx, ns, []string{id}); err != nil {
		return err
	}
	m.forgetProjections(ctx, scope, []string{id})
	return nil
}
