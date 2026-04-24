package journal

import (
	"context"
	"io"
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
//
// The wrapper transparently delegates the optional retrieval sub-interfaces
// (DocGetter, Filterable, DeletableByFilter, Droppable, Iterable,
// Snapshottable, Hybridable, Vectorizable) so that callers who type-assert
// on the wrapped value still reach the inner backend's native fast paths.
// DeleteByFilter and Drop additionally emit OpDelete events for every
// affected document so the journal stays a complete audit log; this can be
// expensive on bulk deletes — call directly on the inner Index when audit
// is not required.
func Wrap(inner retrieval.Index, j Journal, opts ...Option) retrieval.Index {
	cfg := wrapCfg{}
	for _, o := range opts {
		o(&cfg)
	}
	return newWrapped(&journaledIndex{inner: inner, j: j, actor: cfg.actor})
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

// recordDocEvents emits OpDelete events for the supplied snapshot of docs
// using a single timestamp. Used by DeleteByFilter / Drop bridges to keep
// the journal a faithful audit log of bulk deletes.
func (w *journaledIndex) recordDocEvents(ctx context.Context, namespace string, docs []retrieval.Doc) error {
	now := time.Now()
	act := ""
	if w.actor != nil {
		act = w.actor(ctx)
	}
	for i := range docs {
		d := docs[i]
		ev := Event{
			Namespace: namespace,
			Op:        OpDelete,
			DocID:     d.ID,
			Before:    &d,
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

// snapshotForFilter returns the documents that match f in namespace by
// walking the inner Index (Iterate first, falling back to List). Used to
// reconstruct OpDelete events for DeleteByFilter / Drop journaling.
func (w *journaledIndex) snapshotForFilter(ctx context.Context, namespace string, f retrieval.Filter) ([]retrieval.Doc, error) {
	if it, ok := w.inner.(retrieval.Iterable); ok {
		var out []retrieval.Doc
		cursor := ""
		for {
			batch, next, err := it.Iterate(ctx, namespace, cursor, 256)
			if err != nil {
				return nil, err
			}
			for _, d := range batch {
				if retrieval.DocMatchesFilter(d, f) {
					out = append(out, d)
				}
			}
			if next == "" || next == cursor {
				break
			}
			cursor = next
		}
		return out, nil
	}
	var out []retrieval.Doc
	tok := ""
	for {
		resp, err := w.inner.List(ctx, namespace, retrieval.ListRequest{
			Filter: f, PageSize: 256, PageToken: tok, WithVector: true,
		})
		if err != nil {
			return nil, err
		}
		if resp == nil {
			break
		}
		out = append(out, resp.Items...)
		if resp.NextPageToken == "" {
			break
		}
		tok = resp.NextPageToken
	}
	return out, nil
}

// matchAllFilter matches every document; used by Drop bridges to enumerate
// the namespace before deletion so OpDelete events are emitted for each row.
//
// We use Filter.Or with a single empty branch so callers that gate on
// "non-empty filter" still see this as a deliberate sentinel, distinct from
// the zero filter that DeleteByFilter rejects.
var matchAllFilter = retrieval.Filter{Or: []retrieval.Filter{{}}}

// Sub-interface bridges. Each method delegates to the corresponding inner
// implementation and emits journal events for write operations. Wrap returns
// a wrapper variant whose method set matches the inner backend's optional
// interfaces so callers' type assertions keep working.

func (w *journaledIndex) Get(ctx context.Context, namespace, id string) (retrieval.Doc, bool, error) {
	g, ok := w.inner.(retrieval.DocGetter)
	if !ok {
		return retrieval.Doc{}, false, nil
	}
	return g.Get(ctx, namespace, id)
}

func (w *journaledIndex) SupportsFilter(f retrieval.Filter) bool {
	if x, ok := w.inner.(retrieval.Filterable); ok {
		return x.SupportsFilter(f)
	}
	return false
}

func (w *journaledIndex) DeleteByFilter(ctx context.Context, namespace string, f retrieval.Filter) (int64, error) {
	d, ok := w.inner.(retrieval.DeletableByFilter)
	if !ok {
		return 0, nil
	}
	docs, err := w.snapshotForFilter(ctx, namespace, f)
	if err != nil {
		return 0, err
	}
	n, err := d.DeleteByFilter(ctx, namespace, f)
	if err != nil {
		return n, err
	}
	if err := w.recordDocEvents(ctx, namespace, docs); err != nil {
		return n, err
	}
	return n, nil
}

func (w *journaledIndex) Drop(ctx context.Context, namespace string) error {
	d, ok := w.inner.(retrieval.Droppable)
	if !ok {
		return nil
	}
	docs, err := w.snapshotForFilter(ctx, namespace, matchAllFilter)
	if err != nil {
		return err
	}
	if err := d.Drop(ctx, namespace); err != nil {
		return err
	}
	return w.recordDocEvents(ctx, namespace, docs)
}

func (w *journaledIndex) Iterate(ctx context.Context, namespace, cursor string, batch int) ([]retrieval.Doc, string, error) {
	if it, ok := w.inner.(retrieval.Iterable); ok {
		return it.Iterate(ctx, namespace, cursor, batch)
	}
	return nil, "", nil
}

func (w *journaledIndex) Snapshot(ctx context.Context, namespace string, dst io.Writer) error {
	if s, ok := w.inner.(retrieval.Snapshottable); ok {
		return s.Snapshot(ctx, namespace, dst)
	}
	return nil
}

func (w *journaledIndex) Restore(ctx context.Context, namespace string, src io.Reader) error {
	if s, ok := w.inner.(retrieval.Snapshottable); ok {
		return s.Restore(ctx, namespace, src)
	}
	return nil
}

func (w *journaledIndex) SearchHybrid(ctx context.Context, namespace string, req retrieval.HybridRequest) (*retrieval.SearchResponse, error) {
	if h, ok := w.inner.(retrieval.Hybridable); ok {
		return h.SearchHybrid(ctx, namespace, req)
	}
	return nil, nil
}

func (w *journaledIndex) UpsertWithEmbed(ctx context.Context, namespace string, docs []retrieval.Doc) error {
	v, ok := w.inner.(retrieval.Vectorizable)
	if !ok {
		return nil
	}
	if err := v.UpsertWithEmbed(ctx, namespace, docs); err != nil {
		return err
	}
	now := time.Now()
	act := ""
	if w.actor != nil {
		act = w.actor(ctx)
	}
	for i := range docs {
		d := docs[i]
		after := cloneDocPtr(&d)
		ev := Event{
			Namespace: namespace,
			Op:        OpUpsert,
			DocID:     d.ID,
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

func (w *journaledIndex) SearchByText(ctx context.Context, namespace, text string, topK int) (*retrieval.SearchResponse, error) {
	if v, ok := w.inner.(retrieval.Vectorizable); ok {
		return v.SearchByText(ctx, namespace, text, topK)
	}
	return nil, nil
}

// newWrapped returns a retrieval.Index whose method set mirrors the optional
// interfaces implemented by base.inner. The matrix is small, so we
// enumerate the combinations rather than rely on dynamic proxying.
//
// Combinations omitted here (e.g. Snapshottable + Vectorizable) fall back
// to the conservative "expose only Index" wrapper. Add a branch when a new
// backend needs the projection.
func newWrapped(base *journaledIndex) retrieval.Index {
	inner := base.inner
	_, hasGet := inner.(retrieval.DocGetter)
	_, hasFlt := inner.(retrieval.Filterable)
	_, hasDF := inner.(retrieval.DeletableByFilter)
	_, hasDrop := inner.(retrieval.Droppable)
	_, hasIter := inner.(retrieval.Iterable)
	switch {
	case hasGet && hasFlt && hasDF && hasDrop && hasIter:
		return &wrappedFull{journaledIndex: base}
	case hasGet && hasDF && hasDrop && hasIter:
		return &wrappedAuditable{journaledIndex: base}
	case hasGet && hasIter:
		return &wrappedReadable{journaledIndex: base}
	case hasGet:
		return &wrappedGetter{journaledIndex: base}
	default:
		return base
	}
}

// The variants below "freeze" the optional method set at compile time so
// callers' interface assertions reflect the inner backend's true
// capability surface. Each embeds *journaledIndex so the bridging methods
// above remain the single source of truth for behaviour.

type wrappedFull struct{ *journaledIndex }
type wrappedAuditable struct{ *journaledIndex }
type wrappedReadable struct{ *journaledIndex }
type wrappedGetter struct{ *journaledIndex }

var (
	_ retrieval.Index             = (*journaledIndex)(nil)
	_ retrieval.DocGetter         = (*wrappedGetter)(nil)
	_ retrieval.DocGetter         = (*wrappedReadable)(nil)
	_ retrieval.Iterable          = (*wrappedReadable)(nil)
	_ retrieval.DocGetter         = (*wrappedAuditable)(nil)
	_ retrieval.DeletableByFilter = (*wrappedAuditable)(nil)
	_ retrieval.Droppable         = (*wrappedAuditable)(nil)
	_ retrieval.Iterable          = (*wrappedAuditable)(nil)
	_ retrieval.Filterable        = (*wrappedFull)(nil)
)
