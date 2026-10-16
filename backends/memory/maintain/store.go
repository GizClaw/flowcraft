package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/recall"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	docview "github.com/GizClaw/flowcraft/backends/memory/views/document"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const overlayRoot = "maintain/v1/overlay"

// Overlay is the read-path effect for one item: a score multiplier and the
// fact that superseded it. Canonical facts are never edited.
type Overlay struct {
	Factor       float64   `json:"factor,omitempty"`
	SupersededBy string    `json:"superseded_by,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

// overlayDocument is one scope's persisted overlay: the entries together with
// the stamp of the pass that wrote them. A document without a stamp is what a
// deployment before the stamp wrote -- the bare entry map of "maintain-v1" --
// and it is read as stale by definition: those judgments cannot be attributed
// to a policy, so they are not applied to another one's scores.
type overlayDocument struct {
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

// Store persists one overlay document per scope on the KV substrate.
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
// pass without a finding clears the previous effect.
//
// The document is written under the stamp of the pass that produced the plan,
// and a plan whose stamp this store would not read back is refused: writing
// judgments the deployment's own read path ignores is a wiring mistake, not a
// runtime condition.
func (store *Store) Save(ctx context.Context, plan Plan) error {
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
		producer = store.expectedProducer()
	}
	if expected := store.expectedProducer(); producer != expected {
		return fmt.Errorf("maintain: overlay written by %q, this store serves %q", producer, expected)
	}
	overlay := make(map[string]Overlay, len(plan.Supersedes)+len(plan.Decays))
	now := store.clock().UTC()
	for _, entry := range plan.Decays {
		key, err := itemKey(plan.Scope, entry.Conversation, entry.FactID)
		if err != nil {
			return err
		}
		current := overlay[key]
		current.Factor = entry.Score
		current.UpdatedAt = now
		overlay[key] = current
	}
	for _, entry := range plan.Supersedes {
		key, err := itemKey(plan.Scope, entry.Conversation, entry.FactID)
		if err != nil {
			return err
		}
		current := overlay[key]
		current.SupersededBy = entry.SupersededBy
		current.UpdatedAt = now
		if current.Factor == 0 || current.Factor > defaultSupersedeFactor {
			current.Factor = defaultSupersedeFactor
		}
		overlay[key] = current
	}
	data, err := json.Marshal(overlayDocument{ProducerVersion: producer, Entries: overlay})
	if err != nil {
		return fmt.Errorf("maintain: encode overlay: %w", err)
	}
	key, err := overlayKey(plan.Scope)
	if err != nil {
		return err
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("maintain: save overlay: %w", err)
	}
	return nil
}

// Load returns one scope's overlay; a missing overlay is an empty map, and so
// is one another pass wrote. Judgments made under a policy this deployment does
// not run are ignored rather than applied to its scores, and the pass that
// rewrites them is the repair: Status reports the condition before it runs.
func (store *Store) Load(ctx context.Context, scope corememory.Scope) (map[string]Overlay, error) {
	document, found, err := store.read(ctx, scope)
	if err != nil {
		return nil, err
	}
	if !found || document.ProducerVersion != store.expectedProducer() {
		return map[string]Overlay{}, nil
	}
	if document.Entries == nil {
		return map[string]Overlay{}, nil
	}
	return document.Entries, nil
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
	document, found, err := store.read(ctx, scope)
	if err != nil || !found {
		return status, err
	}
	status.Stored = true
	status.StoredProducer = document.ProducerVersion
	status.Entries = len(document.Entries)
	status.Stale = document.ProducerVersion != status.ExpectedProducer
	for _, entry := range document.Entries {
		if entry.UpdatedAt.After(status.UpdatedAt) {
			status.UpdatedAt = entry.UpdatedAt.UTC()
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

// read loads one scope's overlay document as stored, decoding the unstamped
// shape so a document written before the stamp is still countable.
func (store *Store) read(ctx context.Context, scope corememory.Scope) (overlayDocument, bool, error) {
	if store == nil || store.kv == nil {
		return overlayDocument{}, false, errors.New("maintain: store is incomplete")
	}
	if ctx == nil {
		return overlayDocument{}, false, errors.New("maintain: context is required")
	}
	if err := scope.Validate(); err != nil {
		return overlayDocument{}, false, err
	}
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
	if document.ProducerVersion == "" {
		var unstamped map[string]Overlay
		if err := json.Unmarshal(data, &unstamped); err != nil {
			return overlayDocument{}, false, fmt.Errorf("maintain: decode unstamped overlay: %w", err)
		}
		document.Entries = unstamped
	}
	return document, true, nil
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
	Config  Config
	Clock   func() time.Time
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
		conversations[fact.ConversationID] = append(conversations[fact.ConversationID], fact)
	}
	plan, err := DetectWithRecalls(scope, conversations, now, service.Config, receipts)
	if err != nil {
		return empty, err
	}
	plan.IgnoredOverlay = status.Stale && status.Entries > 0
	if err := service.Store.Save(ctx, plan); err != nil {
		return empty, err
	}
	if service.Documents != nil {
		reclaimed, err := service.Documents.RetireScopeBuilds(ctx, scope)
		if err != nil {
			return plan, fmt.Errorf("maintain: reclaim superseded document builds: %w", err)
		}
		plan.ReclaimedChunks = reclaimed
	}
	return plan, nil
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

func overlayKey(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return overlayRoot + "/" + partition, nil
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
