package recall

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Request is the input to Memory.Recall ( + §6.2).
type Request struct {
	Query     string
	TopK      int
	Filter    map[string]any // metadata equality filters merged into pipeline filter
	Now       time.Time      // optional clock injection (for tests / TTL)
	WithStale bool           // if true, do NOT filter expired entries
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
func (m *lt) Recall(ctx context.Context, scope Scope, req Request) ([]Hit, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "memory.recall.recall")
	defer span.End()
	t0 := time.Now()
	defer func() {
		recallDuration.Record(ctx, time.Since(t0).Seconds())
	}()

	if scope.RuntimeID == "" {
		span.RecordError(ErrMissingRuntimeID)
		recallTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "fail"), attribute.String("reason", "scope")))
		return nil, ErrMissingRuntimeID
	}
	if m.cfg.requireUserID && scope.UserID == "" && !m.cfg.allowGlobal {
		span.RecordError(ErrMissingUserID)
		recallTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "fail"), attribute.String("reason", "scope")))
		return nil, ErrMissingUserID
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
	if len(req.Filter) > 0 {
		filter = MergeFilters(filter, retrieval.Filter{Eq: req.Filter})
	}
	ns := NamespaceFor(scope)
	span.SetAttributes(
		attribute.String("runtime_id", scope.RuntimeID),
		attribute.Bool("has_user_id", scope.UserID != ""),
		attribute.Int("top_k", topK),
		attribute.Int("query_len", len(req.Query)),
		attribute.Bool("with_stale", req.WithStale),
	)
	resp, err := m.pipe.Run(ctx, m.idx, ns, retrieval.SearchRequest{
		QueryText: req.Query,
		Filter:    filter,
		TopK:      topK,
	})
	if err != nil {
		span.RecordError(err)
		recallTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "fail"), attribute.String("reason", "pipeline")))
		return nil, err
	}
	out := make([]Hit, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		out = append(out, Hit{
			Entry:  DocToEntry(h.Doc),
			Score:  h.Score,
			Scores: h.Scores,
		})
	}
	span.SetAttributes(attribute.Int("hits", len(out)))
	recallHits.Record(ctx, int64(len(out)))
	recallTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "success")))
	return out, nil
}

// History implements Memory; requires Journal.
func (m *lt) History(ctx context.Context, scope Scope, id string) ([]journal.Event, error) {
	if m.cfg.journal == nil {
		return nil, ErrJournalRequired
	}
	return m.cfg.journal.History(ctx, NamespaceFor(scope), id)
}

// Rollback re-applies the last Upsert recorded before t (or deletes the doc
// when no prior state existed).
func (m *lt) Rollback(ctx context.Context, scope Scope, id string, before time.Time) error {
	if m.cfg.journal == nil {
		return ErrJournalRequired
	}
	ns := NamespaceFor(scope)
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
	return m.idx.Delete(ctx, NamespaceFor(scope), []string{id})
}
