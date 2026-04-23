package journal

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// ActorFn extracts an actor label from ctx (optional).
type ActorFn func(ctx context.Context) string

type wrapCfg struct {
	actor ActorFn
}

// Option configures Wrap.
type Option func(*wrapCfg)

// WithActor sets the actor extractor for journal events.
func WithActor(fn ActorFn) Option {
	return func(c *wrapCfg) { c.actor = fn }
}

// journaledIndex wraps retrieval.Index with a Journal.
//
// v0 ordering: inner Upsert/Delete succeeds first, then Journal.Record, so the
// index never points at state that lacks a corresponding audit entry on crash
// after inner success (journal loss) — acceptable for dev; production SQLite
// journal should use WAL true journal-first.
type journaledIndex struct {
	inner retrieval.Index
	j     Journal
	actor ActorFn
}

// Wrap returns retrieval.Index that records mutations to j after inner success.
func Wrap(inner retrieval.Index, j Journal, opts ...Option) retrieval.Index {
	cfg := wrapCfg{}
	for _, o := range opts {
		o(&cfg)
	}
	return &journaledIndex{inner: inner, j: j, actor: cfg.actor}
}

func (w *journaledIndex) Capabilities() retrieval.Capabilities { return w.inner.Capabilities() }
func (w *journaledIndex) Close() error {
	_ = w.j.Close()
	return w.inner.Close()
}

func (w *journaledIndex) Upsert(ctx context.Context, namespace string, docs []retrieval.Doc) error {
	var befores []*retrieval.Doc
	if g, ok := w.inner.(retrieval.DocGetter); ok {
		for _, d := range docs {
			var before *retrieval.Doc
			if old, ok2, _ := g.Get(ctx, namespace, d.ID); ok2 {
				b := old
				before = &b
			}
			befores = append(befores, before)
		}
	} else {
		for range docs {
			befores = append(befores, nil)
		}
	}
	if err := w.inner.Upsert(ctx, namespace, docs); err != nil {
		return err
	}
	now := time.Now()
	act := ""
	if w.actor != nil {
		act = w.actor(ctx)
	}
	for i, d := range docs {
		after := cloneDocPtr(&d)
		ev := Event{
			Namespace: namespace,
			Op:        OpUpsert,
			DocID:     d.ID,
			Before:    befores[i],
			After:     after,
			Actor:     act,
			Timestamp: now,
		}
		if err := w.j.Record(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func (w *journaledIndex) Delete(ctx context.Context, namespace string, ids []string) error {
	var olds []*retrieval.Doc
	if g, ok := w.inner.(retrieval.DocGetter); ok {
		for _, id := range ids {
			if old, ok2, _ := g.Get(ctx, namespace, id); ok2 {
				o := old
				olds = append(olds, &o)
			} else {
				olds = append(olds, nil)
			}
		}
	} else {
		for range ids {
			olds = append(olds, nil)
		}
	}
	if err := w.inner.Delete(ctx, namespace, ids); err != nil {
		return err
	}
	now := time.Now()
	act := ""
	if w.actor != nil {
		act = w.actor(ctx)
	}
	for i, id := range ids {
		ev := Event{
			Namespace: namespace,
			Op:        OpDelete,
			DocID:     id,
			Before:    olds[i],
			After:     nil,
			Actor:     act,
			Timestamp: now,
		}
		if err := w.j.Record(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func (w *journaledIndex) Search(ctx context.Context, namespace string, req retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return w.inner.Search(ctx, namespace, req)
}

func (w *journaledIndex) List(ctx context.Context, namespace string, req retrieval.ListRequest) (*retrieval.ListResponse, error) {
	return w.inner.List(ctx, namespace, req)
}
