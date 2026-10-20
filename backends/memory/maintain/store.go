package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/profile"
	"github.com/GizClaw/flowcraft/backends/memory/recall"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	docview "github.com/GizClaw/flowcraft/backends/memory/views/document"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const (
	// overlayRoot holds one control record per scope: the pass that wrote the
	// scope's entries and what it left in the buckets. It is what the read
	// path gates on, and it is small enough to read on every request.
	overlayRoot = "maintain/v2/overlay"
	// bucketsRoot holds one document per conversation: the entries of that
	// conversation alone. A request reads the buckets its candidates name
	// instead of the scope's whole overlay, and a pass that judged one
	// conversation rewrites one bucket (see SaveConversations).
	bucketsRoot = "maintain/v2/buckets"
	// legacyOverlayRoot is the pre-shard layout: one document per scope
	// holding every entry. Nothing in this version reads it -- a stored one
	// is reported as stale, and a save of any shape is what clears it.
	legacyOverlayRoot = "maintain/v1/overlay"
)

// Overlay is the read-path effect for one item: a score multiplier and the
// fact that superseded it, with the window the item was current over.
// Canonical facts are never edited.
type Overlay struct {
	Factor       float64 `json:"factor,omitempty"`
	SupersededBy string  `json:"superseded_by,omitempty"`
	// Relation is the single-valued relation the supersede was judged on, and
	// it is empty for a supersede judged by text similarity alone. It is what
	// says which of the two the entry means; the finding itself
	// (maintain.Supersede) carries the reason and the evidence.
	Relation string `json:"relation,omitempty"`
	// ValidAt and InvalidAt are the window the item was current over: dated
	// from ValidAt, replaced at InvalidAt. A host answering "what did we know
	// at time T" reads the entry and compares T against them; the read path
	// itself only ever multiplies the score, so the window changes no
	// retrieval -- it is what makes the penalty explainable and the older
	// state recoverable by time rather than by guesswork.
	ValidAt   time.Time `json:"valid_at,omitempty"`
	InvalidAt time.Time `json:"invalid_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// overlayDocument is one scope's control record: the stamp of the pass that
// wrote the scope's buckets, when it last wrote them, and how many entries
// they hold. The counters describe the buckets; the entries themselves stay
// in them, so a read that needs factors reads the buckets it needs and a
// status check does not have to.
//
// UpdatedAt is when the whole scope was last judged, which only a full save
// does. A partial save keeps it, so the expiry signal keeps measuring the
// full pass a scope is waiting for, not the last conversation touched.
type overlayDocument struct {
	ProducerVersion string    `json:"producer_version"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
	Entries         int       `json:"entries,omitempty"`
}

// bucketDocument is one conversation's entries: the overlay factors of the
// facts maintenance judged in it. A conversation with nothing judged has no
// bucket, which is why a full save deletes the buckets of conversations a
// pass found nothing in.
type bucketDocument struct {
	Entries map[string]Overlay `json:"entries"`
}

// legacyDocument is the pre-shard shape: one scope document holding every
// entry, with or without a stamp.
type legacyDocument struct {
	ProducerVersion string             `json:"producer_version"`
	Entries         map[string]Overlay `json:"entries"`
}

// OverlayDiagnostics reports one scope's overlay against the pass this store
// serves: whether a document is stored, which pass wrote it, and whether the
// read path is applying it.
type OverlayDiagnostics struct {
	ExpectedProducer string `json:"expected_producer"`
	StoredProducer   string `json:"stored_producer,omitempty"`
	Stored           bool   `json:"stored"`
	Stale            bool   `json:"stale"`
	// Entries counts what the stored document holds, stale or not, so a host
	// can see how much an ignored overlay covers.
	Entries int `json:"entries,omitempty"`
	// UpdatedAt is the newest entry time in the stored document: when the
	// pass that wrote it last judged the scope's facts.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// Expired reports a stored overlay this pass wrote but has not refreshed
	// within one half-life. Its factors were measured against a clock at
	// least that old, so the read path is serving decayed scores that are too
	// high until the next pass rewrites them; a host schedules the pass, and
	// the pass is the repair.
	Expired bool `json:"expired,omitempty"`
}

// Store persists one overlay per scope on the KV substrate: a control record
// per scope and one bucket per conversation holding entries.
type Store struct {
	kv    storage.Store
	clock func() time.Time
	// config is the maintenance tuning this store serves. An overlay is read
	// only when it was written by the pass this tuning names.
	config Config
}

// StoreOption configures the overlay store.
type StoreOption func(*Store)

// WithClock replaces the store's clock.
func WithClock(clock func() time.Time) StoreOption {
	return func(store *Store) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// WithConfig sets the maintenance tuning this store serves. The default is the
// default tuning, so a deployment that tunes a Service has to tune the store
// the same way: Save refuses a plan whose stamp it could not read back.
func WithConfig(config Config) StoreOption {
	return func(store *Store) { store.config = config }
}

// NewStore builds a KV-backed overlay store.
func NewStore(kv storage.Store, options ...StoreOption) (*Store, error) {
	if kv == nil {
		return nil, errors.New("maintain: kv store is required")
	}
	store := &Store{kv: kv, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// Save replaces one scope's overlay with the plan's effects. Facts that are
// neither superseded nor decayed are dropped from the overlay, so a later
// pass without a finding clears the previous effect: every bucket of the
// scope the plan does not write is deleted.
//
// The plan is written under the stamp of the pass that produced it, and a plan
// whose stamp this store would not read back is refused: writing judgments the
// deployment's own read path ignores is a wiring mistake, not a runtime
// condition. A stored pre-shard document is cleared at the end, because no
// read reaches it and a save is the repair.
func (store *Store) Save(ctx context.Context, plan Plan) error {
	if err := store.checkSave(ctx, plan); err != nil {
		return err
	}
	grouped, entries, err := store.entries(plan)
	if err != nil {
		return err
	}
	stored, err := store.buckets(ctx, plan.Scope)
	if err != nil {
		return err
	}
	for _, conversation := range stored {
		if _, kept := grouped[conversation]; kept {
			continue
		}
		if err := store.deleteBucket(ctx, plan.Scope, conversation); err != nil {
			return err
		}
	}
	// Sorted so a save writes its buckets in one order whatever order the
	// plan's findings arrived in.
	// Sorted so a save writes its buckets in one order whatever order the
	// plan's findings arrived in.
	for _, conversation := range slices.Sorted(maps.Keys(grouped)) {
		if err := store.writeBucket(ctx, plan.Scope, conversation, grouped[conversation]); err != nil {
			return err
		}
	}
	if err := store.writeControl(ctx, plan.Scope, entries, store.clock().UTC()); err != nil {
		return err
	}
	return store.clearLegacy(ctx, plan.Scope)
}

// SaveConversations replaces only the named conversations' buckets: the
// narrow form for a pass that judged a subset of a scope's facts, so entries
// the pass never looked at are not dropped with the ones it did. Every entry
// of the plan has to belong to one of the named conversations: a plan that
// judged a conversation the caller is not replacing would have its findings
// silently discarded, which is a wiring mistake rather than a state to
// produce.
//
// A partial save requires the scope's control record to be this pass's own.
// Adopting a scope whose stored overlay another pass wrote would make the
// read path serve that pass's buckets under this one's stamp, so the caller
// has to run a full pass first, which Save is.
func (store *Store) SaveConversations(
	ctx context.Context,
	plan Plan,
	conversations []string,
) error {
	if err := store.checkSave(ctx, plan); err != nil {
		return err
	}
	named, err := normalizeConversations(conversations)
	if err != nil {
		return err
	}
	grouped, _, err := store.entries(plan)
	if err != nil {
		return err
	}
	for conversation := range grouped {
		if _, ok := named[conversation]; !ok {
			return fmt.Errorf("maintain: plan judged conversation %q, the save does not replace it", conversation)
		}
	}
	control, found, err := store.readControl(ctx, plan.Scope)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("maintain: a partial save needs this pass's overlay; run a full save first")
	}
	expected := store.expectedProducer()
	if control.ProducerVersion != expected {
		return fmt.Errorf("maintain: overlay written by %q, this store serves %q", control.ProducerVersion, expected)
	}
	total := control.Entries
	for _, conversation := range slices.Sorted(maps.Keys(named)) {
		stored, err := store.readBucket(ctx, plan.Scope, conversation)
		if err != nil {
			return err
		}
		total -= len(stored)
		if bucket := grouped[conversation]; len(bucket) > 0 {
			total += len(bucket)
			if err := store.writeBucket(ctx, plan.Scope, conversation, bucket); err != nil {
				return err
			}
			continue
		}
		// The pass judged this conversation and found nothing: the entries
		// it does not carry are exactly the ones to drop.
		if err := store.deleteBucket(ctx, plan.Scope, conversation); err != nil {
			return err
		}
	}
	if total < 0 {
		return fmt.Errorf("maintain: overlay counts %d entries, its buckets hold more", control.Entries)
	}
	if err := store.writeControl(ctx, plan.Scope, total, control.UpdatedAt); err != nil {
		return err
	}
	return store.clearLegacy(ctx, plan.Scope)
}

// Load returns one scope's overlay; a missing overlay is an empty map, and so
// is one another pass wrote. Judgments made under a policy this deployment does
// not run are ignored rather than applied to its scores, and the pass that
// rewrites them is the repair: Status reports the condition before it runs.
func (store *Store) Load(ctx context.Context, scope corememory.Scope) (map[string]Overlay, error) {
	control, found, err := store.readControl(ctx, scope)
	if err != nil {
		return nil, err
	}
	if !found || control.ProducerVersion != store.expectedProducer() {
		return map[string]Overlay{}, nil
	}
	conversations, err := store.buckets(ctx, scope)
	if err != nil {
		return nil, err
	}
	overlay := make(map[string]Overlay, control.Entries)
	for _, conversation := range conversations {
		bucket, err := store.readBucket(ctx, scope, conversation)
		if err != nil {
			return nil, err
		}
		for identity, entry := range bucket {
			overlay[identity] = entry
		}
	}
	return overlay, nil
}

// ScoreOverlayFor returns the score multipliers of the named conversations,
// and only of them: the read a request with candidates of its own needs. A
// bucket that was never written is not an error -- it is a conversation
// maintenance found nothing in.
func (store *Store) ScoreOverlayFor(
	ctx context.Context,
	scope corememory.Scope,
	conversations []string,
) (map[string]float64, error) {
	if store == nil || store.kv == nil {
		return nil, errors.New("maintain: store is incomplete")
	}
	if ctx == nil {
		return nil, errors.New("maintain: context is required")
	}
	named, err := normalizeConversations(conversations)
	if err != nil {
		return nil, err
	}
	control, found, err := store.readControl(ctx, scope)
	if err != nil {
		return nil, err
	}
	if !found || control.ProducerVersion != store.expectedProducer() {
		return map[string]float64{}, nil
	}
	scores := map[string]float64{}
	for _, conversation := range slices.Sorted(maps.Keys(named)) {
		bucket, err := store.readBucket(ctx, scope, conversation)
		if err != nil {
			return nil, err
		}
		for identity, entry := range bucket {
			if entry.Factor > 0 && entry.Factor < 1 {
				scores[identity] = entry.Factor
			}
		}
	}
	return scores, nil
}

// Status reports one scope's stored overlay: which pass wrote it and whether
// the read path applies it. A host checks this to decide whether to schedule a
// maintenance pass; the pass itself is the repair, since it rewrites the
// overlay under the policy the deployment runs.
func (store *Store) Status(ctx context.Context, scope corememory.Scope) (OverlayDiagnostics, error) {
	status := OverlayDiagnostics{}
	if store == nil || store.kv == nil {
		return status, errors.New("maintain: store is incomplete")
	}
	status.ExpectedProducer = store.expectedProducer()
	document, found, err := store.readControl(ctx, scope)
	if err != nil {
		return status, err
	}
	if found {
		status.Stored = true
		status.StoredProducer = document.ProducerVersion
		status.Entries = document.Entries
		status.UpdatedAt = document.UpdatedAt.UTC()
		status.Stale = document.ProducerVersion != status.ExpectedProducer
	} else {
		// No control record: a document in the pre-shard layout is the one
		// overlay this deployment could still have. It is not read, so it is
		// reported the way any unreadable overlay is -- stale -- and the next
		// save clears it.
		legacy, legacyFound, err := store.readLegacy(ctx, scope)
		if err != nil {
			return status, err
		}
		if !legacyFound {
			return status, nil
		}
		status.Stored = true
		status.StoredProducer = legacy.ProducerVersion
		status.Entries = len(legacy.Entries)
		status.Stale = true
		for _, entry := range legacy.Entries {
			if entry.UpdatedAt.After(status.UpdatedAt) {
				status.UpdatedAt = entry.UpdatedAt.UTC()
			}
		}
	}
	status.Expired = !status.Stale && status.Entries > 0 &&
		store.clock().UTC().Sub(status.UpdatedAt) > store.config.withDefaults().DecayHalfLife
	return status, nil
}

// expectedProducer is the stamp this store reads: the pass a plan has to name
// for its overlay to be served here.
func (store *Store) expectedProducer() string {
	return store.config.withDefaults().ProducerVersion()
}

// checkSave validates the store, the context, and the plan's stamp -- the
// three things both save shapes need before they touch a bucket.
func (store *Store) checkSave(ctx context.Context, plan Plan) error {
	if store == nil || store.kv == nil {
		return errors.New("maintain: store is incomplete")
	}
	if ctx == nil {
		return errors.New("maintain: context is required")
	}
	if err := plan.Scope.Validate(); err != nil {
		return err
	}
	producer := strings.TrimSpace(plan.ProducerVersion)
	if producer == "" {
		// A plan built by hand names no pass: it is written under this store's
		// own, which is the only stamp the store reads back.
		return nil
	}
	if expected := store.expectedProducer(); producer != expected {
		return fmt.Errorf("maintain: overlay written by %q, this store serves %q", producer, expected)
	}
	return nil
}

// entries turns a plan's findings into one entry map per conversation, and
// returns how many entries there are in total.
func (store *Store) entries(plan Plan) (map[string]map[string]Overlay, int, error) {
	grouped := map[string]map[string]Overlay{}
	now := store.clock().UTC()
	put := func(conversation, identity string, update func(*Overlay)) {
		bucket := grouped[conversation]
		if bucket == nil {
			bucket = map[string]Overlay{}
			grouped[conversation] = bucket
		}
		current := bucket[identity]
		update(&current)
		current.UpdatedAt = now
		bucket[identity] = current
	}
	total := 0
	for _, entry := range plan.Decays {
		key, err := itemKey(plan.Scope, entry.Conversation, entry.FactID)
		if err != nil {
			return nil, 0, err
		}
		put(entry.Conversation, key, func(current *Overlay) {
			current.Factor = entry.Score
		})
	}
	for _, entry := range plan.Supersedes {
		key, err := itemKey(plan.Scope, entry.Conversation, entry.FactID)
		if err != nil {
			return nil, 0, err
		}
		// A window is what a host reads to answer "was this true then", so a
		// plan that would publish one running backwards is refused here rather
		// than served: the pass orders a pair by the same dates, and a caller
		// building findings by hand is the one that can get it wrong.
		if !entry.ValidAt.IsZero() && entry.InvalidAt.Before(entry.ValidAt) {
			return nil, 0, fmt.Errorf(
				"maintain: supersede of %q names a window ending before it started (%s..%s)",
				entry.FactID, entry.ValidAt.UTC().Format(time.RFC3339), entry.InvalidAt.UTC().Format(time.RFC3339))
		}
		put(entry.Conversation, key, func(current *Overlay) {
			current.SupersededBy = entry.SupersededBy
			current.Relation = entry.Relation
			current.ValidAt = entry.ValidAt
			current.InvalidAt = entry.InvalidAt
			if current.Factor == 0 || current.Factor > defaultSupersedeFactor {
				current.Factor = defaultSupersedeFactor
			}
		})
	}
	for _, bucket := range grouped {
		total += len(bucket)
	}
	return grouped, total, nil
}

// buckets lists the conversations that hold a bucket in one scope.
func (store *Store) buckets(ctx context.Context, scope corememory.Scope) ([]string, error) {
	prefix, err := bucketsPrefix(scope)
	if err != nil {
		return nil, err
	}
	entries, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("maintain: list overlay buckets: %w", err)
	}
	conversations := make([]string, 0, len(entries))
	for _, entry := range entries {
		segment, ok := strings.CutPrefix(entry.Key, prefix+"/")
		if !ok || strings.Contains(segment, "/") {
			return nil, fmt.Errorf("maintain: unexpected key %q under the bucket area", entry.Key)
		}
		conversation, err := storage.DecodeSegment(segment)
		if err != nil {
			return nil, fmt.Errorf("maintain: decode bucket key %q: %w", entry.Key, err)
		}
		conversations = append(conversations, conversation)
	}
	return conversations, nil
}

func (store *Store) readControl(ctx context.Context, scope corememory.Scope) (overlayDocument, bool, error) {
	key, err := overlayKey(scope)
	if err != nil {
		return overlayDocument{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return overlayDocument{}, false, nil
		}
		return overlayDocument{}, false, fmt.Errorf("maintain: load overlay: %w", err)
	}
	var document overlayDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return overlayDocument{}, false, fmt.Errorf("maintain: decode overlay: %w", err)
	}
	return document, true, nil
}

// writeControl publishes the scope's control record: the stamp, the count, and
// the time of the full pass it describes. A partial save passes the time it
// read back, so only a pass that judged the whole scope moves it.
func (store *Store) writeControl(
	ctx context.Context,
	scope corememory.Scope,
	entries int,
	updatedAt time.Time,
) error {
	key, err := overlayKey(scope)
	if err != nil {
		return err
	}
	document := overlayDocument{
		ProducerVersion: store.expectedProducer(),
		UpdatedAt:       updatedAt.UTC(),
		Entries:         entries,
	}
	data, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("maintain: encode overlay: %w", err)
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("maintain: save overlay: %w", err)
	}
	return nil
}

func (store *Store) readBucket(
	ctx context.Context,
	scope corememory.Scope,
	conversation string,
) (map[string]Overlay, error) {
	key, err := bucketKey(scope, conversation)
	if err != nil {
		return nil, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return map[string]Overlay{}, nil
		}
		return nil, fmt.Errorf("maintain: load overlay bucket: %w", err)
	}
	var document bucketDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("maintain: decode overlay bucket: %w", err)
	}
	if document.Entries == nil {
		return map[string]Overlay{}, nil
	}
	return document.Entries, nil
}

func (store *Store) writeBucket(
	ctx context.Context,
	scope corememory.Scope,
	conversation string,
	entries map[string]Overlay,
) error {
	key, err := bucketKey(scope, conversation)
	if err != nil {
		return err
	}
	data, err := json.Marshal(bucketDocument{Entries: entries})
	if err != nil {
		return fmt.Errorf("maintain: encode overlay bucket: %w", err)
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("maintain: save overlay bucket: %w", err)
	}
	return nil
}

func (store *Store) deleteBucket(ctx context.Context, scope corememory.Scope, conversation string) error {
	key, err := bucketKey(scope, conversation)
	if err != nil {
		return err
	}
	if err := store.kv.Delete(ctx, key); err != nil {
		return fmt.Errorf("maintain: delete overlay bucket: %w", err)
	}
	return nil
}

// readLegacy loads a pre-shard document, decoding the unstamped shape so one
// written before the stamp existed is still countable.
func (store *Store) readLegacy(ctx context.Context, scope corememory.Scope) (legacyDocument, bool, error) {
	key, err := legacyOverlayKey(scope)
	if err != nil {
		return legacyDocument{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return legacyDocument{}, false, nil
		}
		return legacyDocument{}, false, fmt.Errorf("maintain: load overlay: %w", err)
	}
	var document legacyDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return legacyDocument{}, false, fmt.Errorf("maintain: decode overlay: %w", err)
	}
	if document.ProducerVersion == "" {
		var unstamped map[string]Overlay
		if err := json.Unmarshal(data, &unstamped); err != nil {
			return legacyDocument{}, false, fmt.Errorf("maintain: decode unstamped overlay: %w", err)
		}
		document.Entries = unstamped
	}
	return document, true, nil
}

// clearLegacy removes a pre-shard document: nothing reads it, and every save
// is the repair for what a deployment found stored.
func (store *Store) clearLegacy(ctx context.Context, scope corememory.Scope) error {
	key, err := legacyOverlayKey(scope)
	if err != nil {
		return err
	}
	if err := store.kv.Delete(ctx, key); err != nil {
		return fmt.Errorf("maintain: clear pre-shard overlay: %w", err)
	}
	return nil
}

// ScoreOverlay implements the retrieval read path: item identity to score
// multiplier, skipping entries that carry no decay.
func (store *Store) ScoreOverlay(ctx context.Context, scope corememory.Scope) (map[string]float64, error) {
	overlay, err := store.Load(ctx, scope)
	if err != nil {
		return nil, err
	}
	scores := make(map[string]float64, len(overlay))
	for key, entry := range overlay {
		if entry.Factor > 0 && entry.Factor < 1 {
			scores[key] = entry.Factor
		}
	}
	return scores, nil
}

// Adjust implements the single-identity score adjuster form.
func (store *Store) Adjust(ctx context.Context, scope corememory.Scope, identity string) (float64, error) {
	overlay, err := store.Load(ctx, scope)
	if err != nil {
		return 1, err
	}
	entry, ok := overlay[identity]
	if !ok || entry.Factor <= 0 || entry.Factor >= 1 {
		return 1, nil
	}
	return entry.Factor, nil
}

// RecallSource reports the read path's receipts for one scope: hard-scope
// item identity to how often and how recently a context result returned it.
// A service without a source plans from event times alone, and the plan says
// so by reporting zero recalled facts.
type RecallSource interface {
	Receipts(context.Context, corememory.Scope) (map[string]recall.Receipt, error)
}

// Service is the host-invoked batch entry point: it lists one scope's facts,
// reads the read path's receipts, plans maintenance, saves the overlay, and
// reclaims the document builds the scope no longer serves.
type Service struct {
	Facts *factview.FactStore
	Store *Store
	// Documents, when set, is the document chunk view whose superseded builds a
	// pass reclaims. It is optional: a deployment without derived documents has
	// nothing to reclaim, and a host that would rather sweep them separately can
	// call DocumentViewStore.RetireScopeBuilds itself.
	Documents *docview.DocumentViewStore
	// Recalls, when set, is the receipt store the retrieval provider writes
	// to. It is what lets the pass tell a fact that keeps being served from
	// one that was imported and never read; without it every fact decays from
	// its event time.
	Recalls RecallSource
	// Profiles, when set, is the resident block store this pass fills: every
	// conversation it judges gets its distilled half replaced with the facts
	// that state a profile line, minus the ones this pass superseded. It is
	// the same judgment the overlay is, written where a request reads it
	// without retrieving, so the two are planned and saved by one pass and
	// cannot disagree about which fact is current.
	Profiles *profile.Store
	Config   Config
	Clock    func() time.Time
}

// RunScope plans and saves one scope's maintenance overlay, then reclaims the
// superseded document builds of the scope.
//
// The overlay is saved before the builds are reclaimed, so a pass that fails
// while reclaiming still publishes what it detected, and the reclaim is
// idempotent: a retried pass finds less to remove. A failed reclaim is returned
// with the plan, since the plan it was going to report was already applied.
//
// A pass also reports what it replaced: an overlay an earlier version or tuning
// wrote is ignored by the read path, and Plan.IgnoredOverlay says so, which is
// the flag a host logs and the pass clears.
func (service *Service) RunScope(ctx context.Context, scope corememory.Scope) (Plan, error) {
	return service.run(ctx, scope, nil)
}

// RunConversations plans and saves the named conversations of one scope and
// leaves the judgments of the conversations it was not given where they are:
// the incremental pass a host runs when it knows which conversations changed.
//
// Supersede judgments are made inside a conversation -- facts of two
// conversations are never compared -- so re-judging the changed conversations
// judges exactly what a whole pass would judge them to be, at the cost of
// those conversations instead of the scope. Decay is the part that does not
// stay fresh this way: a conversation left behind keeps the factors the last
// full pass measured, which is the condition the overlay's expiry signal
// reports (OverlayDiagnostics.Expired, measured from the scope's last full
// pass), and the full pass is the repair.
//
// A scope whose stored overlay this pass cannot read is rebuilt whole: an
// incremental pass has no judgments to keep, and the caller would otherwise
// be the one that has to run the full pass first.
func (service *Service) RunConversations(
	ctx context.Context,
	scope corememory.Scope,
	conversations []string,
) (Plan, error) {
	named, err := normalizeConversations(conversations)
	if err != nil {
		return Plan{}, err
	}
	if len(named) == 0 {
		return Plan{}, errors.New("maintain: at least one conversation is required")
	}
	return service.run(ctx, scope, named)
}

// run is both pass shapes: judged names the conversations to judge, or nil for
// the whole scope. A scope whose stored overlay this pass cannot read is
// rebuilt whole whatever was named, so an incremental caller never has to know
// the scope's history before it can ask.
func (service *Service) run(
	ctx context.Context,
	scope corememory.Scope,
	judged map[string]struct{},
) (Plan, error) {
	empty := Plan{}
	if service == nil || service.Facts == nil || service.Store == nil {
		return empty, errors.New("maintain: service requires facts and a store")
	}
	if ctx == nil {
		return empty, errors.New("maintain: context is required")
	}
	if err := scope.Validate(); err != nil {
		return empty, err
	}
	now := time.Now().UTC()
	if service.Clock != nil {
		now = service.Clock().UTC()
	}
	// What the read path serves before this pass: an overlay another policy
	// wrote is not applied to this one's scores, and this pass is what
	// replaces it.
	status, err := service.Store.Status(ctx, scope)
	if err != nil {
		return empty, err
	}
	readable := status.Stored && !status.Stale
	if judged != nil && !readable {
		// Nothing readable to keep: this pass replaces the scope's overlay,
		// which is the full pass the caller would have to run next.
		judged = nil
	}
	facts, err := service.Facts.ListScope(ctx, scope)
	if err != nil {
		return empty, fmt.Errorf("maintain: list facts: %w", err)
	}
	receipts, err := service.recallReceipts(ctx, scope)
	if err != nil {
		return empty, err
	}
	conversations := make(map[string][]factview.Fact)
	for _, fact := range facts {
		if strings.TrimSpace(fact.ConversationID) == "" {
			continue
		}
		if judged != nil {
			if _, wanted := judged[fact.ConversationID]; !wanted {
				continue
			}
		}
		conversations[fact.ConversationID] = append(conversations[fact.ConversationID], fact)
	}
	plan, err := DetectWithRecalls(scope, conversations, now, service.Config, receipts)
	if err != nil {
		return empty, err
	}
	plan.IgnoredOverlay = status.Stale && status.Entries > 0
	if judged == nil {
		err = service.Store.Save(ctx, plan)
	} else {
		// Every named conversation is replaced, including the ones the pass
		// found nothing in: a conversation judged as fresh is exactly the
		// state that has to drop the factors an earlier pass wrote for it.
		err = service.Store.SaveConversations(ctx, plan, slices.Sorted(maps.Keys(judged)))
	}
	if err != nil {
		return empty, err
	}
	if service.Documents != nil {
		reclaimed, err := service.Documents.RetireScopeBuilds(ctx, scope)
		if err != nil {
			return plan, fmt.Errorf("maintain: reclaim superseded document builds: %w", err)
		}
		plan.ReclaimedChunks = reclaimed
	}
	if err := service.fillProfiles(ctx, scope, conversations, plan, now); err != nil {
		return plan, err
	}
	return plan, nil
}

// fillProfiles replaces the distilled half of every judged conversation's
// resident block with what the facts of this pass say: the lines whose text
// states a profile class, newest first inside each class, minus the facts this
// pass superseded -- the block states what holds now, and a superseded fact is
// the state before. A conversation the pass judged and found nothing to say
// gets an empty half, which is how a block drops a line the corpus stopped
// supporting.
//
// The block is written from the same facts and the same judgment as the
// overlay, in the same pass, so a request cannot read a line whose fact the
// overlay already called replaced. A pass that cannot write the blocks reports
// the error with the plan it already saved: the overlay is what the read path
// applies, and a block left behind carries its own policy stamp, so the next
// pass repairs it rather than serving it as this one's judgment.
func (service *Service) fillProfiles(
	ctx context.Context,
	scope corememory.Scope,
	conversations map[string][]factview.Fact,
	plan Plan,
	now time.Time,
) error {
	if service.Profiles == nil || len(conversations) == 0 {
		return nil
	}
	config := service.Profiles.Config()
	if !config.Distill {
		// The block's pass-written half is not served under this store's
		// policy, so a pass that filled it would be writing what nothing
		// reads.
		return nil
	}
	superseded := make(map[string]map[string]struct{}, len(plan.Supersedes))
	for _, finding := range plan.Supersedes {
		if _, exists := superseded[finding.Conversation]; !exists {
			superseded[finding.Conversation] = make(map[string]struct{})
		}
		superseded[finding.Conversation][finding.FactID] = struct{}{}
	}
	blocks := make(map[string][]profile.Entry, len(conversations))
	for conversationID, facts := range conversations {
		blocks[conversationID] = profile.Distill(facts, superseded[conversationID], config)
	}
	if _, err := service.Profiles.SaveDistilledConversations(ctx, scope, blocks, now); err != nil {
		return fmt.Errorf("maintain: fill profile blocks: %w", err)
	}
	return nil
}

// recallReceipts reads the receipt index, treating an unwired source as a
// scope the read path never served. A store that is wired but unreadable
// fails the pass: planning without the receipts would decay exactly the facts
// the caller keeps using.
func (service *Service) recallReceipts(ctx context.Context, scope corememory.Scope) (map[string]recall.Receipt, error) {
	source := service.Recalls
	if source == nil {
		return nil, nil
	}
	reflected := reflect.ValueOf(source)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if reflected.IsNil() {
			return nil, nil
		}
	}
	receipts, err := source.Receipts(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("maintain: read recall receipts: %w", err)
	}
	return receipts, nil
}

const defaultSupersedeFactor = 0.5

// sortedKeys returns the keys of a map in ascending order, so a save writes
// its buckets -- and the entries inside them -- in one order whatever order
// the plan's findings arrived in.
// normalizeConversations turns a caller's conversation list into the set a
// save or a scoped read works in: trimmed, deduplicated, and never empty. A
// read with candidates of its own names a conversation per candidate, so
// repeats are the caller's normal shape rather than a mistake.
func normalizeConversations(conversations []string) (map[string]struct{}, error) {
	named := make(map[string]struct{}, len(conversations))
	for _, conversation := range conversations {
		trimmed := strings.TrimSpace(conversation)
		if trimmed == "" {
			return nil, errors.New("maintain: conversation id is required")
		}
		named[trimmed] = struct{}{}
	}
	return named, nil
}

func overlayKey(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return overlayRoot + "/" + partition, nil
}

func bucketsPrefix(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return bucketsRoot + "/" + partition, nil
}

func bucketKey(scope corememory.Scope, conversationID string) (string, error) {
	prefix, err := bucketsPrefix(scope)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(conversationID) == "" {
		return "", errors.New("maintain: conversation id is required")
	}
	return prefix + "/" + storage.EncodeSegment(conversationID), nil
}

func legacyOverlayKey(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return legacyOverlayRoot + "/" + partition, nil
}

func itemKey(scope corememory.Scope, conversationID, factID string) (string, error) {
	if strings.TrimSpace(conversationID) == "" || strings.TrimSpace(factID) == "" {
		return "", errors.New("maintain: conversation and fact id are required")
	}
	address := corememory.ContextAddress{
		Kind: corememory.ContextFact, ConversationID: conversationID, ItemID: factID,
	}
	return address.Identity(scope), nil
}
