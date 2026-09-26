package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
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

// Store persists one overlay document per scope on the KV substrate.
type Store struct {
	kv    storage.Store
	clock func() time.Time
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
	data, err := json.Marshal(overlay)
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

// Load returns one scope's overlay; a missing overlay is an empty map.
func (store *Store) Load(ctx context.Context, scope corememory.Scope) (map[string]Overlay, error) {
	if store == nil || store.kv == nil {
		return nil, errors.New("maintain: store is incomplete")
	}
	if ctx == nil {
		return nil, errors.New("maintain: context is required")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	key, err := overlayKey(scope)
	if err != nil {
		return nil, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return map[string]Overlay{}, nil
		}
		return nil, fmt.Errorf("maintain: load overlay: %w", err)
	}
	var overlay map[string]Overlay
	if err := json.Unmarshal(data, &overlay); err != nil {
		return nil, fmt.Errorf("maintain: decode overlay: %w", err)
	}
	return overlay, nil
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

// Service is the host-invoked batch entry point: it lists one scope's facts,
// plans maintenance, and saves the overlay.
type Service struct {
	Facts  *factview.FactStore
	Store  *Store
	Config Config
	Clock  func() time.Time
}

// RunScope plans and saves one scope's maintenance overlay.
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
	facts, err := service.Facts.ListScope(ctx, scope)
	if err != nil {
		return empty, fmt.Errorf("maintain: list facts: %w", err)
	}
	conversations := make(map[string][]factview.Fact)
	for _, fact := range facts {
		if strings.TrimSpace(fact.ConversationID) == "" {
			continue
		}
		conversations[fact.ConversationID] = append(conversations[fact.ConversationID], fact)
	}
	plan, err := DetectWithConfig(scope, conversations, now, service.Config)
	if err != nil {
		return empty, err
	}
	if err := service.Store.Save(ctx, plan); err != nil {
		return empty, err
	}
	return plan, nil
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
