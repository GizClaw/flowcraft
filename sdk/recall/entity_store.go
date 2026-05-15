package recall

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// EntityStore is a per-scope inverted index keyed by normalized entity
// name. Each row aggregates the memory entry IDs in which the entity
// appears, giving the recall pipeline an exhaustive lookup channel for
// entity-anchored questions (e.g. "what books has Alice read") that
// the vector / BM25 / entity-filter lanes cannot match.
//
// EntityStore is an INTERNAL accelerator: callers never see its rows.
// All access goes through [Memory.Save] (which writes the inverted
// index alongside the entry writes) and the EntityLinkLookup pipeline
// stage (which reads). The interface is exported only so tests can
// substitute a fake without faking the underlying retrieval.Index.
//
// Backed by a retrieval.Index under a sibling namespace
// ([EntityNamespaceFor]). The composite row ID is
// "<scope.UserID>::<normalized_name>" — see [EntityKey] — so the
// same name in two different user scopes does not collide.
type EntityStore interface {
	// Link adds memEntryIDs to the entities' linked_ids lists. New
	// entities are created with an initial list; existing entities
	// have their lists merged (deduplicated, capped at the
	// configured maxLinkedIDs with FIFO eviction of the oldest).
	//
	// Idempotent on the (entity, memID) tuple — pairs already
	// present after dedup are no-ops, so a Save retry that triggered
	// this twice does not double-count the entity's linked count.
	//
	// All entity row writes happen in a single retrieval.Index.Upsert
	// batch — entity_store row writes share the same atomic
	// semantics as the entry writes that triggered them. Errors fall
	// through to the caller; the recall.lt.upsertFacts path treats
	// the failure as best-effort (entries remain written; entity-link
	// lane just misses these entries this turn).
	Link(ctx context.Context, scope Scope, entityToIDs map[string][]string) error

	// Lookup returns the union of linked memory IDs for the given
	// query entities under scope. Order is deterministic — entities
	// are normalized + sorted, then ids appear in their insertion
	// order per entity — so downstream RRF rank assignment is
	// stable.
	//
	// perEntityCap caps the ids drawn from each entity to bound the
	// candidate set; 0 = no cap (the entire stored list). The cap is
	// applied recency-first (the LAST n entries of the FIFO list),
	// because the most recent linked ids are the freshest evidence
	// about the entity.
	//
	// Missing entities are silently skipped (returns empty slice for
	// entities the store has never seen). A nil/empty entities slice
	// returns an empty result without round-tripping the backend.
	Lookup(ctx context.Context, scope Scope, entities []string, perEntityCap int) ([]string, error)

	// Forget removes memEntryID from every entity's linked_ids that
	// references it. Called by Memory.Forget so the inverted index
	// never accumulates dangling pointers. A no-op when the id is
	// not referenced by any entity row under this scope.
	Forget(ctx context.Context, scope Scope, memEntryID string) error
}

// EntityNamespaceFor returns the retrieval namespace for the entity
// store sibling table of a given scope. The encoding mirrors
// [NamespaceFor] and appends "__entities" so backends that support
// prefix-scan can drop both the entry namespace and its entity
// sibling in one pass.
//
//	entries:   "ltm_default__u_conv-26"
//	entities:  "ltm_default__u_conv-26__entities"
//
// The suffix lives in the saneNS character set ([A-Za-z0-9_]) so
// adapter-side namespace validation (sqlite/postgres §6.2/§6.3)
// continues to pass.
func EntityNamespaceFor(s Scope) string {
	return NamespaceFor(s) + "__entities"
}

// EntityKey returns the canonical retrieval.Doc.ID for the given
// (scope, name) inside the entity store namespace. Encoding:
//
//	"<saneNS(scope.UserID)>::<normalize(name)>"
//
// Composite-key rationale:
//
//   - UserID first so backends that prefix-scan stay cheap per-user
//     even when the entire entity table is unified across scopes.
//   - "::" delimiter — different from the namespace separator "__"
//     so a row ID can be quoted in error messages without confusion;
//     also impossible to collide with normalize() output (which
//     strips punctuation including ':').
//   - The UserID is fed through [saneNS] for the same reason
//     [NamespaceFor] does: two UserIDs that the namespace function
//     considers equivalent (e.g. differing only by characters outside
//     the saneNS alphabet) must produce the same row. Otherwise the
//     entry namespace would already collide them while the entity
//     namespace would not — Lookup would silently miss those entries.
//   - scope.UserID == "" (global scope) sane's to "anon"; same
//     partitioning convention as [NamespaceFor].
//
// LoCoMo evaluations bind scope.UserID == conversation_id, so
// "conv-26::alice" and "conv-30::alice" automatically isolate
// distinct Alices across conversations. Production multi-user
// deployments inherit the same isolation through scope.UserID ==
// the end-user identifier.
func EntityKey(s Scope, name string) string {
	return entityKeyPrefix(s) + normalizeEntityName(name)
}

// entityKeyPrefix returns the "<saneNS(UserID)>::" prefix that all
// rows for a scope share. Centralised so EntityStore.Link / Lookup
// and the internal resolver agree on a single encoding rule.
func entityKeyPrefix(s Scope) string {
	u := s.UserID
	if u == "" {
		u = "anon"
	}
	return saneNS(u) + "::"
}

// ScopeFromNamespace reverses [NamespaceFor]: given an entry
// namespace, it returns the Scope coordinates encoded into the
// namespace string. Because saneNS is lossy, the returned values
// are the SANE'D forms of the original RuntimeID / UserID — not the
// pristine inputs. Callers that need to round-trip through this
// function (specifically the pipeline-side EntityLinkResolver) must
// keep their EntityKey encoding aligned with the sane'd values; the
// [EntityKey] helper does so by construction.
//
// Recognised shapes (matching the [NamespaceFor] grammar):
//
//	"ltm_<rt>__u_<user>"   ⇒ Scope{RuntimeID: <rt>, UserID: <user>}
//	"ltm_<rt>__global"     ⇒ Scope{RuntimeID: <rt>}
//
// Strings that do not match either grammar return (Scope{}, false).
// The grammar deliberately does not accept the "__entities" sibling
// suffix — that namespace is INTERNAL to the EntityStore; the
// resolver is invoked with the ENTRY namespace and looks up its
// sibling itself via [EntityNamespaceFor].
func ScopeFromNamespace(ns string) (Scope, bool) {
	const prefix = "ltm_"
	if !strings.HasPrefix(ns, prefix) {
		return Scope{}, false
	}
	rest := ns[len(prefix):]
	// Prefer the per-user shape: split on the unique "__u_" infix
	// so a runtime name that happens to contain "__" stays intact
	// (saneNS allows underscores, and double-underscore in saneNS
	// output cannot be produced by collapsing characters because
	// saneNS replaces one rune at a time).
	if i := strings.LastIndex(rest, "__u_"); i >= 0 {
		return Scope{
			RuntimeID: rest[:i],
			UserID:    rest[i+len("__u_"):],
		}, true
	}
	if strings.HasSuffix(rest, "__global") {
		return Scope{
			RuntimeID: rest[:len(rest)-len("__global")],
		}, true
	}
	return Scope{}, false
}

// normalizeEntityName produces the canonical lookup key for an
// entity. It is intentionally simpler than [NormalizeEntities]:
// EntityStore stores ONE row per phrase as the LLM extractor
// produced it (after lowercasing / trimming). The atomization step
// that turns "Alice's LGBTQ support group" into individual atoms
// happens at WRITE time (upsertFacts feeds normalized atoms back
// through this function) and at READ time (ruleEntities emits the
// atoms the pipeline asks for).
//
// Doing the atomization upstream of EntityKey lets every (atom →
// linked_ids) pair share one row, which is what the lookup channel
// needs. Doing it inside EntityKey would mean each call had to
// guess "is this a phrase or an atom?" and possibly write multiple
// rows for one input; the upstream caller is in the better position
// to make that decision.
func normalizeEntityName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	// Strip surrounding punctuation; keep internal apostrophes and
	// hyphens (NormalizeEntities handles atom splitting later).
	s = strings.TrimFunc(s, isTrimmableRune)
	// Strip the English possessive 's so "alice's" and "alice"
	// converge on the same key — matches NormalizeEntities's atom
	// path.
	s = strings.TrimSuffix(s, "'s")
	s = strings.TrimSuffix(s, "\u2019s")
	return s
}

// IndexEntityStoreOptions configures a default-backed EntityStore.
type IndexEntityStoreOptions struct {
	// LinkedCap is the upper bound on per-entity linked_ids length.
	// Zero falls back to defaultEntityLinkedCap. FIFO eviction —
	// when a Link would push the list past this length the OLDEST
	// id is dropped. The dropped id's entry remains in the entry
	// namespace; only the entity-link lane loses it.
	LinkedCap int

	// Clock is the time source for MetaEntityLast. Defaults to
	// time.Now. Tests use a fixed clock to assert ordering without
	// depending on wall time.
	Clock func() time.Time
}

// defaultEntityLinkedCap caps per-entity linked_ids at 200. The
// choice is empirical: LoCoMo single-conv ingest produces ~300 facts,
// a heavy entity rarely appears in more than 100 of them, so 200
// gives a 2× safety margin. At ~26 B per ULID + 4 B overhead per
// list cell the metadata row sits under 6 KB, well within sqlite /
// postgres JSON-blob limits.
//
// Re-evaluate if Phase-1 LoCoMo ablation shows the cap is thrashing
// (signal: MetaEntityCount stuck at 200 across many entities); pass
// [WithEntityStoreLinkedCap] to override at construction time.
const defaultEntityLinkedCap = 200

// IndexEntityStore is the default [EntityStore] implementation. It
// persists rows in a retrieval.Index under [EntityNamespaceFor],
// using the row layout pinned in the package-level metadata
// constants ([MetaEntityName], [MetaEntityLinked], [MetaEntityCount],
// [MetaEntityLast]).
//
// The backing index MUST implement [retrieval.DocGetter] so per-key
// Link / Lookup can avoid an O(N) namespace walk; all in-tree
// backends (memory / sqlite / postgres / fs) satisfy this contract.
// NewIndexEntityStore checks at construction time and returns nil
// when the index does not implement DocGetter — callers should fall
// back to "feature disabled" rather than crash the recall pipeline.
//
// The store is stateless beyond the backing index — concurrent
// Link calls touching DIFFERENT entities are race-free because they
// land in different Doc IDs. Concurrent Link calls touching the
// SAME entity row may race; the last writer wins. This is
// acceptable because Save's per-batch atomicity is at the entry
// level (one transcript turn = one Save), and within one Save the
// caller already constructs a single entityToIDs map.
type IndexEntityStore struct {
	idx       retrieval.Index
	getter    retrieval.DocGetter
	linkedCap int
	now       func() time.Time
}

// NewIndexEntityStore returns a retrieval.Index-backed EntityStore.
// Returns nil when idx does not implement [retrieval.DocGetter];
// callers (typically [New] when WithEntityStore is set) should
// degrade to "feature disabled" rather than panic.
func NewIndexEntityStore(idx retrieval.Index, opts IndexEntityStoreOptions) *IndexEntityStore {
	if idx == nil {
		return nil
	}
	g, ok := idx.(retrieval.DocGetter)
	if !ok {
		return nil
	}
	cap := opts.LinkedCap
	if cap <= 0 {
		cap = defaultEntityLinkedCap
	}
	now := opts.Clock
	if now == nil {
		now = time.Now
	}
	return &IndexEntityStore{idx: idx, getter: g, linkedCap: cap, now: now}
}

// Link implements EntityStore.
func (s *IndexEntityStore) Link(ctx context.Context, scope Scope, entityToIDs map[string][]string) error {
	if s == nil || s.idx == nil {
		return nil
	}
	if len(entityToIDs) == 0 {
		return nil
	}
	ns := EntityNamespaceFor(scope)
	now := s.now().UnixMilli()

	// 1. Read existing rows for every entity we're touching so we
	// can merge linked_ids deterministically. Backends without
	// native batch-get fall through to N Get calls — acceptable
	// because |entityToIDs| is bounded by facts*entities_per_fact
	// (~5-20 in LoCoMo).
	existing := make(map[string]retrieval.Doc, len(entityToIDs))
	for raw := range entityToIDs {
		key := EntityKey(scope, raw)
		doc, ok, err := s.getter.Get(ctx, ns, key)
		if err != nil {
			return fmt.Errorf("entity_store: read %q: %w", key, err)
		}
		if ok {
			existing[raw] = doc
		}
	}

	// 2. Merge new ids into existing linked_ids; build the Upsert
	// batch. We use a separate slice rather than mutating Doc in
	// place to keep the backing slice stable in case the index
	// shares storage with the read response.
	docs := make([]retrieval.Doc, 0, len(entityToIDs))
	keys := sortedKeys(entityToIDs)
	for _, raw := range keys {
		newIDs := entityToIDs[raw]
		if len(newIDs) == 0 {
			continue
		}
		key := EntityKey(scope, raw)
		var display string
		var linked []string
		if d, ok := existing[raw]; ok {
			display = entityDisplayName(d)
			linked = entityLinkedSlice(d)
		}
		if display == "" {
			display = strings.TrimSpace(raw)
		}
		// Append-with-dedup. Walking a small set is cheaper than
		// allocating a map here — LinkedCap is at most a few
		// hundred, and the dedup probe inside the loop is O(N) per
		// candidate which still beats the map allocation cost.
		seen := make(map[string]struct{}, len(linked))
		for _, id := range linked {
			seen[id] = struct{}{}
		}
		for _, id := range newIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			linked = append(linked, id)
		}
		// FIFO evict: trim the oldest prefix so len <= linkedCap.
		if over := len(linked) - s.linkedCap; over > 0 {
			linked = linked[over:]
		}
		docs = append(docs, retrieval.Doc{
			ID:      key,
			Content: display,
			Metadata: map[string]any{
				MetaEntityName:   display,
				MetaEntityLinked: linked,
				MetaEntityCount:  int64(len(linked)),
				MetaEntityLast:   now,
			},
		})
	}
	if len(docs) == 0 {
		return nil
	}
	return s.idx.Upsert(ctx, ns, docs)
}

// Lookup implements EntityStore.
func (s *IndexEntityStore) Lookup(ctx context.Context, scope Scope, entities []string, perEntityCap int) ([]string, error) {
	if s == nil || s.idx == nil || len(entities) == 0 {
		return nil, nil
	}
	ns := EntityNamespaceFor(scope)
	// Normalize + dedup entity inputs so callers don't have to.
	seen := make(map[string]struct{}, len(entities))
	keys := make([]string, 0, len(entities))
	for _, e := range entities {
		n := normalizeEntityName(e)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		keys = append(keys, n)
	}
	sort.Strings(keys) // deterministic order for RRF rank stability
	out := make([]string, 0, len(keys)*8)
	added := make(map[string]struct{}, cap(out))
	prefix := entityKeyPrefix(scope)
	for _, k := range keys {
		doc, ok, err := s.getter.Get(ctx, ns, prefix+k)
		if err != nil {
			return nil, fmt.Errorf("entity_store: lookup %q: %w", k, err)
		}
		if !ok {
			continue
		}
		linked := entityLinkedSlice(doc)
		if perEntityCap > 0 && len(linked) > perEntityCap {
			// Recency-first cap: keep the LAST perEntityCap ids
			// (most recent under the FIFO write order).
			linked = linked[len(linked)-perEntityCap:]
		}
		for _, id := range linked {
			if _, dup := added[id]; dup {
				continue
			}
			added[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out, nil
}

// Forget implements EntityStore. It walks every entity row under
// scope and rewrites those that reference memEntryID. Backends
// without native filter pushdown still page through List in chunks
// so memory is bounded.
func (s *IndexEntityStore) Forget(ctx context.Context, scope Scope, memEntryID string) error {
	if s == nil || s.idx == nil || memEntryID == "" {
		return nil
	}
	ns := EntityNamespaceFor(scope)
	now := s.now().UnixMilli()
	// We can't filter on "metadata.entity_linked contains X" in a
	// portable way (Contains is a metadata-filter primitive that
	// backends implement variably for list-typed values), so we
	// page through every entity row under the namespace and
	// rewrite the ones that match. Forget is rare relative to Link,
	// so the O(N) scan is acceptable.
	var page string
	for {
		req := retrieval.ListRequest{PageSize: 500, PageToken: page}
		resp, err := s.idx.List(ctx, ns, req)
		if err != nil || resp == nil {
			return err
		}
		var toUpsert []retrieval.Doc
		for _, d := range resp.Items {
			linked := entityLinkedSlice(d)
			pruned := make([]string, 0, len(linked))
			changed := false
			for _, id := range linked {
				if id == memEntryID {
					changed = true
					continue
				}
				pruned = append(pruned, id)
			}
			if !changed {
				continue
			}
			if d.Metadata == nil {
				d.Metadata = map[string]any{}
			}
			d.Metadata[MetaEntityLinked] = pruned
			d.Metadata[MetaEntityCount] = int64(len(pruned))
			d.Metadata[MetaEntityLast] = now
			toUpsert = append(toUpsert, d)
		}
		if len(toUpsert) > 0 {
			if err := s.idx.Upsert(ctx, ns, toUpsert); err != nil {
				return fmt.Errorf("entity_store: forget rewrite: %w", err)
			}
		}
		if resp.NextPageToken == "" {
			return nil
		}
		page = resp.NextPageToken
	}
}

// entityLinkedSlice extracts MetaEntityLinked from a Doc, tolerating
// both []string (native) and []any (after backend JSON round-trip).
func entityLinkedSlice(d retrieval.Doc) []string {
	if d.Metadata == nil {
		return nil
	}
	v, ok := d.Metadata[MetaEntityLinked]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		out := make([]string, len(t))
		copy(out, t)
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// entityDisplayName returns MetaEntityName, falling back to
// Doc.Content when the metadata is absent (e.g. a future migration
// path that recovers from corrupted metadata).
func entityDisplayName(d retrieval.Doc) string {
	if d.Metadata != nil {
		if v, ok := d.Metadata[MetaEntityName].(string); ok && v != "" {
			return v
		}
	}
	return strings.TrimSpace(d.Content)
}

// sortedKeys returns the keys of m in sorted order. Centralised so
// EntityStore writes a deterministic Upsert batch — backends that
// log writes (or compare-and-swap on order) see the same sequence
// in tests and prod.
func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Compile-time interface check.
var _ EntityStore = (*IndexEntityStore)(nil)
