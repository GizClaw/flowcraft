package recall

import (
	"context"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
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
func (m *lt) Save(ctx context.Context, scope Scope, msgs []llm.Message) (SaveResult, error) {
	if err := m.validateScope(scope); err != nil {
		return SaveResult{}, err
	}
	m.rememberNamespace(ctx, NamespaceFor(scope))
	var extractOpts []ExtractOption
	if m.cfg.saveWithCtx {
		if existing := m.gatherExistingFacts(ctx, scope, msgs); len(existing) > 0 {
			extractOpts = append(extractOpts, WithExistingFacts(existing))
		}
	}
	facts, err := m.cfg.extractor.Extract(ctx, scope, msgs, extractOpts...)
	if err != nil {
		return SaveResult{}, err
	}
	ids, err := m.upsertFacts(ctx, scope, msgs, facts, m.cfg.now())
	if err != nil {
		return SaveResult{}, err
	}
	return SaveResult{EntryIDs: ids, Facts: facts}, nil
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
// string is the assigned entry ID (content-addressable when the caller
// leaves Entry.ID empty).
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
	ns := NamespaceFor(scope)
	m.rememberNamespace(ctx, ns)
	if err := m.idx.Upsert(ctx, ns, []retrieval.Doc{d}); err != nil {
		span.RecordError(err)
		addTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "fail"), attribute.String("reason", "upsert")))
		return "", err
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
	type plan struct {
		entry Entry
		doc   retrieval.Doc
		fact  ExtractedFact
		vec   []float32
	}
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
			Entities:   f.Entities,
			Confidence: f.Confidence,
			CreatedAt:  now,
			UpdatedAt:  now,
			Source: Source{
				RuntimeID: scope.RuntimeID,
				Timestamp: now,
			},
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
		// Slot fields are written only when BOTH Subject and Predicate are
		// present so that the slot supersede channel and the SlotCollapse
		// retrieval stage can rely on slot_key being unambiguous.
		//
		// Subjects/predicates that contain the slot delimiter '|' would
		// produce ambiguous slot_keys (e.g. subject="user|alt"+
		// predicate="lives_in" would collide with subject="user"+
		// predicate="alt|lives_in"). The Save path drops the slot
		// fields entirely in that case so the fact degrades to the
		// vector / resolver supersede channels — slot writers stay
		// strict, the rest of the pipeline keeps working.
		if slotEligible(f) {
			d.Metadata[MetaSubject] = f.Subject
			d.Metadata[MetaPredicate] = f.Predicate
			d.Metadata[MetaSlotKey] = f.Subject + slotDelimiter + f.Predicate
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

	// 3. Supersede channels (slot + vector) -------------------------------
	// supersedeNeighbours dispatches between the two channels and
	// honours WithoutSlotChannel / WithoutSoftMerge independently;
	// always call it and let the dispatcher decide.
	for _, p := range plans {
		m.supersedeNeighbours(ctx, scope, p.entry.ID, p.fact, p.vec, now)
	}

	// 4. LLM update resolver (opt-in fallback) -----------------------------
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

	// 5. Upsert new docs ---------------------------------------------------
	docs := make([]retrieval.Doc, len(plans))
	for i, p := range plans {
		docs[i] = p.doc
	}
	if err := m.idx.Upsert(ctx, ns, docs); err != nil {
		return nil, err
	}
	return returnedIDs, nil
}
