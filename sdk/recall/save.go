package recall

import (
	"context"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// embedBatch wraps Embedder.EmbedBatch with a defensive fallback to per-text
// Embed for legacy implementations that may panic on empty / short inputs.
func embedBatch(ctx context.Context, emb embedding.Embedder, texts []string) ([][]float32, error) {
	if emb == nil || len(texts) == 0 {
		return nil, nil
	}
	vecs, err := emb.EmbedBatch(ctx, texts)
	if err == nil && len(vecs) == len(texts) {
		return vecs, nil
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, e := emb.Embed(ctx, t)
		if e != nil {
			return out, e
		}
		out[i] = v
	}
	return out, nil
}

// SaveResult is the outcome of a synchronous Save call.
type SaveResult struct {
	EntryIDs []string
	Facts    []ExtractedFact
}

// plan is the per-fact bookkeeping record threaded through
// upsertFacts and its post-write hooks (linkEntities). It is
// package-level so the hooks can accept []plan without exposing
// retrieval.Doc / ExtractedFact wiring to callers.
type plan struct {
	entry Entry
	doc   retrieval.Doc
	fact  ExtractedFact
	vec   []float32
}

// validateScope is enforced by every write path.
func (m *lt) validateScope(s Scope) error {
	if s.RuntimeID == "" {
		return ErrMissingRuntimeID
	}
	if m.cfg.requireUserID && s.UserID == "" && !m.cfg.allowGlobal {
		return ErrMissingUserID
	}
	return nil
}

// Save (sync) extracts then upserts; returns generated entry IDs.
//
// The durable portion (validate → extract → upsert) runs through
// the shared [executeWrite] pipeline so the sync and async
// (handleJob) paths cannot diverge on validation, extractor
// options, or upsert ordering. The post-success history append
// remains here because its ctx-lifecycle differs from the async
// caller's (sync reuses the user ctx; async uses a fresh
// bookkeeping ctx — see handleJob for rationale).
func (m *lt) Save(ctx context.Context, scope Scope, msgs []llm.Message) (SaveResult, error) {
	ids, facts, err := m.executeWrite(ctx, scope, msgs, m.cfg.now())
	if err != nil {
		return SaveResult{}, err
	}
	// Append AFTER persistence: a Save that failed mid-extract must
	// not poison the next Save's context. Append errors are also
	// non-fatal — the history store is a quality booster, not part
	// of the durability contract.
	if m.cfg.historyStore != nil {
		m.appendHistory(ctx, NamespaceFor(scope), msgs)
	}
	return SaveResult{EntryIDs: ids, Facts: facts}, nil
}

// buildExtractOpts assembles the WithRecentMessages / WithExistingFacts
// extractor options from the configured history store and save-with-
// context settings. Both blocks are best-effort quality boosters: a
// missing history store, an empty recent window, or a recall failure
// degrade extraction without breaking the save.
//
// Shared between Save (sync) and handleJob (SaveAsync worker) so the
// async ingest path honours WithHistoryStore / WithRecentMessagesK /
// WithSaveWithCtx identically to the sync path (issue #149).
func (m *lt) buildExtractOpts(ctx context.Context, scope Scope, ns string, msgs []llm.Message) []ExtractOption {
	var extractOpts []ExtractOption
	// Recent turns must be read BEFORE the current msgs are appended
	// so the extractor only ever sees PRIOR conversational context —
	// the current batch goes into the CONVERSATION slot, not the
	// RECENT TURNS slot.
	if m.cfg.historyStore != nil && m.cfg.recentMsgsK > 0 {
		if recent := m.readRecentHistory(ctx, ns, m.cfg.recentMsgsK); len(recent) > 0 {
			extractOpts = append(extractOpts, WithRecentMessages(recent))
		}
	}
	if m.cfg.saveWithCtx {
		if existing := m.gatherExistingFacts(ctx, scope, msgs); len(existing) > 0 {
			extractOpts = append(extractOpts, WithExistingFacts(existing))
		}
	}
	return extractOpts
}

// readRecentHistory pulls the last k messages from the configured
// history.Store, preferring the cheap RecentReader path when the
// store implements it. The conversation key is the recall namespace
// so one Scope maps to one conversation; multi-conversation
// deployments should partition Scopes accordingly.
//
// llm.Message is an alias for model.Message (sdk/llm/aliases.go), so
// no shape conversion is needed at the package boundary.
func (m *lt) readRecentHistory(ctx context.Context, namespace string, k int) []llm.Message {
	if rr, ok := m.cfg.historyStore.(history.RecentReader); ok {
		recent, err := rr.GetRecentMessages(ctx, namespace, k)
		if err != nil {
			m.log("recall: history GetRecentMessages: %v", err)
			return nil
		}
		return recent
	}
	all, err := m.cfg.historyStore.GetMessages(ctx, namespace)
	if err != nil {
		m.log("recall: history GetMessages: %v", err)
		return nil
	}
	if k < len(all) {
		all = all[len(all)-k:]
	}
	return all
}

// appendHistory writes the current Save's messages into the store,
// preferring the incremental AppendMessages path when available so
// large conversations don't incur a full re-write per Save.
//
// Concurrency: the fallback read-modify-write path holds the
// per-namespace [KeyedMutex] entry across the GetMessages /
// SaveMessages pair so two concurrent same-namespace Saves cannot
// interleave (issue #154 RMW race). Stores that implement
// MessageAppender are presumed to provide their own atomicity and
// do NOT acquire the KeyedMutex — the cost (one map probe per
// Save) is paid only where it is needed.
func (m *lt) appendHistory(ctx context.Context, namespace string, msgs []llm.Message) {
	if len(msgs) == 0 {
		return
	}
	if ap, ok := m.cfg.historyStore.(history.MessageAppender); ok {
		if err := ap.AppendMessages(ctx, namespace, msgs); err != nil {
			m.log("recall: history AppendMessages: %v", err)
		}
		return
	}
	// Fallback: read-modify-write. Acceptable only for stores that
	// cannot offer MessageAppender; the per-namespace mutex makes
	// the pair atomic so two concurrent same-namespace Saves cannot
	// clobber each other's appends.
	m.historyAppendMu.Lock(namespace)
	defer m.historyAppendMu.Unlock(namespace)
	existing, _ := m.cfg.historyStore.GetMessages(ctx, namespace)
	if err := m.cfg.historyStore.SaveMessages(ctx, namespace, append(existing, msgs...)); err != nil {
		m.log("recall: history SaveMessages: %v", err)
	}
}

// gatherExistingFacts runs a best-effort Recall using the conversation as
// query, returning short snippets to seed the extractor's "existing memories"
// section. Errors are swallowed: this is a quality booster, not correctness-
// critical.
func (m *lt) gatherExistingFacts(ctx context.Context, scope Scope, msgs []llm.Message) []string {
	if len(msgs) == 0 {
		return nil
	}
	q := joinMessageText(msgs, 800)
	if q == "" {
		return nil
	}
	hits, err := m.Recall(ctx, scope, Request{
		Query: q,
		TopK:  m.cfg.saveCtxTopK,
	})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		s := snippetSingleLine(h.Entry.Content, 200)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func joinMessageText(msgs []llm.Message, max int) string {
	var b []byte
	for _, m := range msgs {
		c := m.Content()
		if c == "" {
			continue
		}
		if len(b) > 0 {
			b = append(b, '\n')
		}
		b = append(b, c...)
		if len(b) >= max {
			b = b[:max]
			break
		}
	}
	return string(b)
}

func snippetSingleLine(s string, max int) string {
	out := make([]rune, 0, max)
	prevSpace := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		out = append(out, r)
		if len(out) >= max {
			out = append(out, '…')
			break
		}
	}
	return string(out)
}

// SaveAsync enqueues; returns immediately with JobID.
func (m *lt) SaveAsync(ctx context.Context, scope Scope, msgs []llm.Message) (JobID, error) {
	if err := m.validateScope(scope); err != nil {
		return "", err
	}
	ns := NamespaceFor(scope)
	m.rememberNamespace(ctx, ns)
	return m.cfg.jobQueue.Enqueue(ctx, ns, JobPayload{Scope: scope, Messages: msgs})
}

// Add bypasses extraction and writes one entry verbatim. The returned
// string is the assigned entry ID.
//
// Add stamps the per-namespace content hash on the doc so a later
// Save's MD5-dedup probe can recognise verbatim-written entries and
// avoid producing a duplicate (issue #163). Add itself does NOT run
// the dedup probe — two Add calls with identical (scope, content)
// still insert two docs, matching the long-standing "Add is a raw
// write, Save is the de-duplicating ingest path" contract. Callers
// that want idempotent writes should funnel through Save.
//
// Telemetry: emits span memory.recall.add with attributes
// runtime_id / has_user_id / has_vector, increments counter
// memory.recall.add_total{outcome=success|fail}, and records latency
// in histogram memory.recall.add_duration. Scope.UserID is intentionally
// not attached as an attribute (PII + cardinality).
func (m *lt) Add(ctx context.Context, scope Scope, e Entry) (string, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "memory.recall.add")
	defer span.End()
	t0 := time.Now()
	defer func() {
		addDuration.Record(ctx, time.Since(t0).Seconds())
	}()

	if err := m.validateScope(scope); err != nil {
		span.RecordError(err)
		addTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "fail"), attribute.String("reason", "scope")))
		return "", err
	}
	// Validate content before any derived work (ID hash, timestamps,
	// embedding) so an empty payload fails fast and never shows up in
	// debug logs or telemetry attributes.
	if e.Content == "" {
		err := errors.New("recall: Add: content is required")
		span.RecordError(err)
		addTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "fail"), attribute.String("reason", "empty_content")))
		return "", err
	}
	now := m.cfg.now()
	if e.ID == "" {
		e.ID = deterministicEntryID(scope, nil, 0, e.Content+"|"+now.Format(time.RFC3339Nano))
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = now
	}
	e.Scope = scope
	if e.Source.RuntimeID == "" {
		e.Source.RuntimeID = scope.RuntimeID
		e.Source.Timestamp = now
	}
	if e.ExpiresAt == nil && m.cfg.ttlPolicy != nil {
		if d, ok := m.cfg.ttlPolicy.TTLFor(e); ok && d > 0 {
			t := now.Add(d)
			e.ExpiresAt = &t
		}
	}
	d := EntryToDoc(e)
	// Stamp the per-namespace content hash so a subsequent Save's
	// dedupHashes probe (sdk/recall/merger.go) recognises this entry
	// as already-present and short-circuits the duplicate write
	// (issue #163). Mirrors the unconditional stamp in upsertFacts.
	if d.Metadata == nil {
		d.Metadata = map[string]any{}
	}
	d.Metadata[MetaContentHash] = contentHash(scope, e.Content)
	ns := NamespaceFor(scope)
	m.rememberNamespace(ctx, ns)
	if err := m.idx.Upsert(ctx, ns, []retrieval.Doc{d}); err != nil {
		span.RecordError(err)
		addTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "fail"), attribute.String("reason", "upsert")))
		return "", err
	}
	// Slot supersede runs AFTER the new doc lands (issue #167). The
	// "d.ID == newID" guard inside supersedeBySlot keeps the new
	// entry from super-seding itself when the post-upsert List sees
	// it. Running after Upsert means an Upsert failure can never
	// leave orphan MetaSupersededBy pointers on older slot-mates.
	if m.cfg.slotMerge && entrySlotEligible(e.Subject, e.Predicate) {
		m.supersedeBySlot(ctx, scope, e.ID,
			ExtractedFact{Subject: e.Subject, Predicate: e.Predicate}, now)
	}
	span.SetAttributes(
		attribute.String("runtime_id", scope.RuntimeID),
		attribute.Bool("has_user_id", scope.UserID != ""),
		attribute.Bool("has_vector", len(d.Vector) > 0),
	)
	addTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "success")))
	return e.ID, nil
}

// upsertFacts is the shared write path used by both Save and async workers.
//
// Pipeline:
//  1. Optional MD5 dedup: drop facts whose md5(scope.UserID|content) is
//     already in the namespace.
//  2. Embed each surviving fact (single batch when Embedder is provided).
//  3. Optional soft-merge: mark older near-duplicates as superseded_by=newID.
//  4. Upsert the new docs.
func (m *lt) upsertFacts(
	ctx context.Context, scope Scope, msgs []llm.Message,
	facts []ExtractedFact, now time.Time,
) ([]string, error) {
	if len(facts) == 0 {
		return nil, nil
	}
	ns := NamespaceFor(scope)
	m.rememberNamespace(ctx, ns)

	// 0. Normalize slot fields --------------------------------------------
	// Predicate / Subject aliases are applied here (not in the extractor)
	// so per-instance overrides reach every fact regardless of which
	// extractor produced it. Running normalisation at the very top of
	// upsertFacts also means every downstream step (MD5 dedup, slot
	// metadata writing, slot supersede, resolver candidate gathering)
	// observes the same canonical (subject, predicate) tuple — there
	// is exactly one source of truth for what a fact "is about".
	for i := range facts {
		facts[i].Subject = normalizeSubject(facts[i].Subject, m.cfg.subjectAliases)
		facts[i].Predicate = normalizePredicate(facts[i].Predicate, m.cfg.predicateAliases)
	}

	// 1. MD5 dedup ---------------------------------------------------------
	hashes := make([]string, len(facts))
	for i, f := range facts {
		hashes[i] = contentHash(scope, f.Content)
	}
	var existing map[string]string
	if m.cfg.md5Dedup {
		var err error
		existing, err = m.dedupHashes(ctx, scope, hashes)
		if err != nil {
			m.log("ltm: md5 dedup probe failed: %v", err)
			existing = nil
		}
	}

	// 2. Build entries + embed --------------------------------------------
	plans := make([]plan, 0, len(facts))
	// returnedIDs tracks the ID we report back per fact, including those
	// short-circuited by MD5 dedup so callers see idempotent behaviour.
	returnedIDs := make([]string, 0, len(facts))
	for i, f := range facts {
		if existing != nil {
			if existingID, ok := existing[hashes[i]]; ok {
				returnedIDs = append(returnedIDs, existingID)
				continue
			}
		}
		entry := Entry{
			ID:         deterministicEntryID(scope, msgs, i, f.Content),
			Scope:      scope,
			Content:    f.Content,
			Categories: f.Categories,
			// NormalizeEntities folds the LLM-supplied phrasal entities
			// (e.g. "Caroline's LGBTQ support group") into the same
			// lower-cased, per-token-atom key space that the
			// retrieval pipeline's rule-based query extractor uses
			// — without it the entity recall lane silently degrades
			// to zero recall because the stored phrase and the
			// per-token query atom never share a string.
			Entities:   NormalizeEntities(f.Entities),
			Confidence: f.Confidence,
			CreatedAt:  now,
			UpdatedAt:  now,
			Source: Source{
				RuntimeID: scope.RuntimeID,
				Timestamp: now,
			},
			// Subject / Predicate flow through Entry so EntryToDoc is the
			// single source of truth for slot metadata writes. Eligibility
			// (both fields set, neither containing '|') is enforced inside
			// EntryToDoc; ineligible tuples silently degrade and the fact
			// falls back to the vector / resolver supersede channels —
			// same contract the slot writer had before this refactor.
			Subject:   f.Subject,
			Predicate: f.Predicate,
		}
		if len(entry.Categories) > 0 {
			entry.Category = Category(entry.Categories[0])
		}
		if m.cfg.ttlPolicy != nil {
			if d, ok := m.cfg.ttlPolicy.TTLFor(entry); ok && d > 0 {
				t := now.Add(d)
				entry.ExpiresAt = &t
			}
		}
		d := EntryToDoc(entry)
		if d.Metadata == nil {
			d.Metadata = map[string]any{}
		}
		d.Metadata[MetaContentHash] = hashes[i]
		if f.Source != "" {
			d.Metadata[MetaSourceLabel] = f.Source
		}
		plans = append(plans, plan{entry: entry, doc: d, fact: f})
		returnedIDs = append(returnedIDs, entry.ID)
	}

	if len(plans) == 0 {
		return returnedIDs, nil
	}

	// Embed in one batch when the embedder is available so each Save is
	// bounded to a single embedding-API round-trip.
	if m.cfg.embedder != nil {
		texts := make([]string, len(plans))
		for i, p := range plans {
			texts[i] = p.fact.Content
		}
		vecs, err := embedBatch(ctx, m.cfg.embedder, texts)
		if err != nil {
			m.log("ltm: embed batch failed: %v", err)
		} else {
			for i := range plans {
				if i < len(vecs) {
					plans[i].vec = vecs[i]
					plans[i].doc.Vector = vecs[i]
				}
			}
		}
	}

	// 3. Upsert new docs FIRST (issue #167) -------------------------------
	// Supersede channels and the LLM update resolver write
	// MetaSupersededBy / MetaTombstone metadata on OLD docs that
	// reference the NEW entry IDs we are about to write. Running
	// those steps BEFORE Upsert would leave dangling pointers on
	// the index if the Upsert below fails: old docs already point
	// at entry IDs that never existed.
	//
	// Reordering: write the new doc batch first, THEN run the
	// supersede + resolver passes. The self-skip guards inside
	// supersedeBySlot / supersedeNeighbours / runResolverBatch
	// (each checks d.ID == newID) guarantee the new entries are
	// not super-seded by themselves on the post-upsert scan, so
	// correctness is preserved while #167's "Upsert-fail leaves
	// orphan supersede tags" hazard goes away.
	docs := make([]retrieval.Doc, len(plans))
	for i, p := range plans {
		docs[i] = p.doc
	}
	if err := m.idx.Upsert(ctx, ns, docs); err != nil {
		return nil, err
	}

	// 4. Supersede channels (slot + vector) -------------------------------
	// supersedeNeighbours dispatches between the two channels and
	// honours WithoutSlotChannel / WithoutSoftMerge independently;
	// always call it and let the dispatcher decide. Failures here
	// leave the new entries written and the older entries
	// un-superseded — the worst case is recall returning a near-
	// duplicate, which is preferable to a dangling pointer.
	for _, p := range plans {
		m.supersedeNeighbours(ctx, scope, p.entry.ID, p.fact, p.vec, now)
	}

	// 5. LLM update resolver (opt-in fallback) -----------------------------
	// The resolver runs at most once per Save with the full batch of
	// non-slot facts so it can reason about combined contradictions
	// (e.g. divorce + remarriage in the same turn). Slot-eligible facts
	// were already handled by supersedeBySlot above and are excluded
	// from the batch.
	if m.cfg.updateResolver != nil {
		batch := make([]ResolveNewFact, 0, len(plans))
		for _, p := range plans {
			if slotEligible(p.fact) {
				continue
			}
			batch = append(batch, ResolveNewFact{EntryID: p.entry.ID, Fact: p.fact})
		}
		if len(batch) > 0 {
			m.runResolverBatch(ctx, scope, batch, now)
		}
	}

	// 6. Entity-link inverted index ---------------------------------------
	// Best-effort: a Link failure logs but does NOT roll back the entry
	// writes above. The entry namespace is the durability boundary;
	// the entity sibling namespace is a retrieval accelerator and can
	// be rebuilt offline by replaying the entries. This matches the
	// existing "embedder failed -> entry still written" contract.
	m.linkEntities(ctx, scope, plans)

	return returnedIDs, nil
}

// linkEntities flushes the entity → entry-id edges produced by the
// current upsertFacts batch into the configured EntityStore. The
// edge set is rebuilt from p.entry.Entities (which is already the
// NormalizeEntities-atomized form, so the same atoms the read-side
// query extractor emits) — no extra normalization happens here so
// write-time and read-time keys stay byte-identical.
//
// Same-entity duplicates within a batch collapse into one Link list
// (the EntityStore further dedupes against existing rows). Empty or
// nil entity slices are skipped silently — facts without entities
// remain reachable through the vector / BM25 lanes.
func (m *lt) linkEntities(ctx context.Context, scope Scope, plans []plan) {
	if m.cfg.entityStore == nil || len(plans) == 0 {
		return
	}
	edges := make(map[string][]string, len(plans))
	for _, p := range plans {
		for _, e := range p.entry.Entities {
			if e == "" {
				continue
			}
			edges[e] = append(edges[e], p.entry.ID)
		}
	}
	if len(edges) == 0 {
		return
	}
	if err := m.cfg.entityStore.Link(ctx, scope, edges); err != nil {
		m.log("ltm: entity_store Link failed: %v", err)
	}
}
