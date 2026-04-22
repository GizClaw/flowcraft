package ltm

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
)

// RecallRequest is the input to Memory.Recall ( + §6.2).
type RecallRequest struct {
	Query     string
	TopK      int
	Filter    map[string]any // metadata equality filters merged into pipeline filter
	Now       time.Time      // optional clock injection (for tests / TTL)
	WithStale bool           // if true, do NOT filter expired entries
}

// RecallHit is one result returned by Memory.Recall.
type RecallHit struct {
	Entry  MemoryEntry
	Score  float64
	Scores map[string]float64
}

// Recall runs the configured pipeline against the namespace and projects
// hits back into MemoryEntry.
func (m *lt) Recall(ctx context.Context, scope MemoryScope, req RecallRequest) ([]RecallHit, error) {
	if scope.RuntimeID == "" {
		return nil, ErrMissingRuntimeID
	}
	if m.cfg.RequireUserID && scope.UserID == "" && !m.cfg.AllowGlobal {
		return nil, ErrMissingUserID
	}
	now := req.Now
	if now.IsZero() {
		now = m.cfg.Now()
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
	resp, err := m.pipe.Run(ctx, m.idx, ns, retrieval.SearchRequest{
		QueryText: req.Query,
		Filter:    filter,
		TopK:      topK,
	})
	if err != nil {
		return nil, err
	}
	out := make([]RecallHit, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		out = append(out, RecallHit{
			Entry:  DocToEntry(h.Doc),
			Score:  h.Score,
			Scores: h.Scores,
		})
	}
	return out, nil
}

// History implements Memory; requires Journal.
func (m *lt) History(ctx context.Context, scope MemoryScope, id string) ([]journal.Event, error) {
	if m.cfg.Journal == nil {
		return nil, ErrJournalRequired
	}
	return m.cfg.Journal.History(ctx, NamespaceFor(scope), id)
}

// Rollback re-applies the last Upsert recorded before t (or deletes the doc
// when no prior state existed).
func (m *lt) Rollback(ctx context.Context, scope MemoryScope, id string, before time.Time) error {
	if m.cfg.Journal == nil {
		return ErrJournalRequired
	}
	ns := NamespaceFor(scope)
	events, err := m.cfg.Journal.History(ctx, ns, id)
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
func (m *lt) Forget(ctx context.Context, scope MemoryScope, id, reason string) error {
	_ = reason // reason is captured by Journal actor (caller can WithActor)
	return m.idx.Delete(ctx, NamespaceFor(scope), []string{id})
}
