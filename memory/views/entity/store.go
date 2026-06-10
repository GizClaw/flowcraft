package entity

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	profileErrPrefix  = "memory/views/entity/profile"
	timelineErrPrefix = "memory/views/entity/timeline"
)

// ProfileID is a stable identifier for an entity profile record.
type ProfileID string

// Slot is a named profile attribute grounded in supporting facts.
type Slot struct {
	Name       string
	Value      string
	Confidence float64
	FactRefs   []fact.FactRef
	Metadata   map[string]any
}

// ProfileRecord summarizes one entity with grounded slots and lineage.
type ProfileRecord struct {
	ID         ProfileID
	EntityID   fact.NodeID
	Label      string
	Summary    string
	Slots      []Slot
	FactRefs   []fact.FactRef
	SourceRefs []views.SourceRef
	Signature  views.ViewSignature
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Metadata   map[string]any
}

// ProfileListOptions controls deterministic profile scans.
type ProfileListOptions struct {
	AfterID  ProfileID
	Limit    int
	EntityID fact.NodeID
	Label    string
}

// ProfileStore persists entity profile records.
type ProfileStore interface {
	Put(ctx context.Context, record ProfileRecord) (ProfileRecord, error)
	Get(ctx context.Context, id ProfileID) (ProfileRecord, bool, error)
	List(ctx context.Context, opts ProfileListOptions) ([]ProfileRecord, error)
	Delete(ctx context.Context, id ProfileID) error
	DeleteEntity(ctx context.Context, entityID fact.NodeID) error
}

// EventID is a stable identifier for an entity timeline event.
type EventID string

// Event records one grounded temporal entity fact cluster.
type Event struct {
	ID          EventID
	EntityID    fact.NodeID
	Title       string
	Description string
	OccurredAt  *time.Time
	ValidFrom   *time.Time
	ValidUntil  *time.Time
	FactRefs    []fact.FactRef
	SourceRefs  []views.SourceRef
	Signature   views.ViewSignature
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Metadata    map[string]any
}

// TimelineListOptions controls deterministic event scans.
type TimelineListOptions struct {
	AfterID  EventID
	Limit    int
	EntityID fact.NodeID
}

// TimelineStore persists entity timeline events.
type TimelineStore interface {
	Put(ctx context.Context, event Event) (Event, error)
	Get(ctx context.Context, id EventID) (Event, bool, error)
	List(ctx context.Context, opts TimelineListOptions) ([]Event, error)
	Delete(ctx context.Context, id EventID) error
	DeleteEntity(ctx context.Context, entityID fact.NodeID) error
}

func validateProfileRecord(record ProfileRecord) error {
	if record.ID == "" {
		return errdefs.Validationf("%s: profile id is required", profileErrPrefix)
	}
	if err := validateEntityID(profileErrPrefix, record.EntityID); err != nil {
		return err
	}
	if record.Label == "" {
		return errdefs.Validationf("%s: label is required", profileErrPrefix)
	}
	if len(record.FactRefs) == 0 {
		return errdefs.Validationf("%s: fact_refs are required", profileErrPrefix)
	}
	for _, ref := range record.FactRefs {
		if err := validateFactRef(profileErrPrefix, ref); err != nil {
			return err
		}
	}
	for _, ref := range record.SourceRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	for _, slot := range record.Slots {
		if err := validateSlot(slot); err != nil {
			return err
		}
	}
	return validateEntitySignature(profileErrPrefix, record.Signature)
}

func validateSlot(slot Slot) error {
	if slot.Name == "" {
		return errdefs.Validationf("%s: slot name is required", profileErrPrefix)
	}
	if slot.Value == "" {
		return errdefs.Validationf("%s: slot value is required", profileErrPrefix)
	}
	if slot.Confidence != 0 && (slot.Confidence < 0 || slot.Confidence > 1) {
		return errdefs.Validationf("%s: slot confidence must be between 0 and 1", profileErrPrefix)
	}
	for _, ref := range slot.FactRefs {
		if err := validateFactRef(profileErrPrefix, ref); err != nil {
			return err
		}
	}
	return nil
}

func validateProfileID(id ProfileID) error {
	if id == "" {
		return errdefs.Validationf("%s: profile id is required", profileErrPrefix)
	}
	return nil
}

func validateEvent(event Event) error {
	if event.ID == "" {
		return errdefs.Validationf("%s: event id is required", timelineErrPrefix)
	}
	if err := validateEntityID(timelineErrPrefix, event.EntityID); err != nil {
		return err
	}
	if event.Title == "" {
		return errdefs.Validationf("%s: title is required", timelineErrPrefix)
	}
	if event.ValidFrom != nil && event.ValidUntil != nil && event.ValidUntil.Before(*event.ValidFrom) {
		return errdefs.Validationf("%s: valid_until must be greater than or equal to valid_from", timelineErrPrefix)
	}
	if len(event.FactRefs) == 0 {
		return errdefs.Validationf("%s: fact_refs are required", timelineErrPrefix)
	}
	for _, ref := range event.FactRefs {
		if err := validateFactRef(timelineErrPrefix, ref); err != nil {
			return err
		}
	}
	for _, ref := range event.SourceRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	return validateEntitySignature(timelineErrPrefix, event.Signature)
}

func validateEventID(id EventID) error {
	if id == "" {
		return errdefs.Validationf("%s: event id is required", timelineErrPrefix)
	}
	return nil
}

func validateEntityID(prefix string, entityID fact.NodeID) error {
	if entityID == "" {
		return errdefs.Validationf("%s: entity id is required", prefix)
	}
	return nil
}

func validateFactRef(prefix string, ref fact.FactRef) error {
	if ref.FactID == "" {
		return errdefs.Validationf("%s: fact ref fact id is required", prefix)
	}
	return nil
}

func validateEntitySignature(prefix string, signature views.ViewSignature) error {
	if signature.IsZero() {
		return errdefs.Validationf("%s: signature is required", prefix)
	}
	if len(signature.UpstreamViewRefs) == 0 {
		return errdefs.Validationf("%s: upstream view refs are required", prefix)
	}
	if err := signature.Validate(); err != nil {
		return err
	}
	return nil
}
