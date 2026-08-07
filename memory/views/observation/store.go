// Package observation stores immutable lifecycle observations and events.
package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const schemaVersion = 1

type State string

const (
	StateActive     State = "active"
	StateSuperseded State = "superseded"
)

func (state State) Validate() error {
	switch state {
	case StateActive, StateSuperseded:
		return nil
	default:
		return fmt.Errorf("observation view: unknown state %q", state)
	}
}

type EventKind string

const (
	EventIntegrated EventKind = "integrated"
	EventSuperseded EventKind = "superseded"
	EventRetention  EventKind = "retention_scored"
	EventVisibility EventKind = "visibility_changed"
)

func (kind EventKind) Validate() error {
	switch kind {
	case EventIntegrated, EventSuperseded, EventRetention, EventVisibility:
		return nil
	default:
		return fmt.Errorf("observation view: unknown event kind %q", kind)
	}
}

type Observation struct {
	ID             string                `json:"id"`
	Scope          sdkmemory.Scope       `json:"scope"`
	Key            string                `json:"key"`
	FactID         string                `json:"fact_id"`
	ConversationID string                `json:"conversation_id"`
	State          State                 `json:"state"`
	Replaces       string                `json:"replaces,omitempty"`
	ReplacedBy     string                `json:"replaced_by,omitempty"`
	Provenance     []sdkmemory.SourceRef `json:"provenance,omitempty"`
	SourceDigest   string                `json:"source_digest,omitempty"`
	EventTime      time.Time             `json:"event_time,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
}

type Event struct {
	ID            string          `json:"id"`
	Scope         sdkmemory.Scope `json:"scope"`
	Kind          EventKind       `json:"kind"`
	ObservationID string          `json:"observation_id"`
	RelatedID     string          `json:"related_id,omitempty"`
	TaskID        string          `json:"task_id"`
	Time          time.Time       `json:"time"`
}

// Option configures an ObservationStore.
type Option func(*ObservationStore)

// WithClock replaces the clock used for authoritative timestamps.
func WithClock(clock func() time.Time) Option {
	return func(store *ObservationStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// CanonicalKey returns the stable observation identity for one fact.
func CanonicalKey(value factview.Fact) string {
	entities := factview.NormalizeEntities(value.Entities)
	predicate := factview.CanonicalContent(value.Predicate)
	if len(entities) > 0 && predicate != "" {
		return "entity-predicate:" + strings.Join(entities, "\x1f") + ":" + predicate
	}
	fallback := strings.TrimSpace(value.CanonicalHash)
	if fallback == "" {
		fallback = value.ID
	}
	return "fallback:" + fallback
}

// ValidateReplacementChain rejects self-replacement and replacement cycles.
func ValidateReplacementChain(values []Observation) error {
	parents := make(map[string]string, len(values))
	for _, value := range values {
		if value.ID == "" {
			return errors.New("observation view: replacement id is required")
		}
		if value.Replaces == value.ID {
			return errors.New("observation view: self replacement")
		}
		parents[value.ID] = value.Replaces
	}
	for id := range parents {
		seen := map[string]struct{}{}
		for current := id; current != ""; current = parents[current] {
			if _, duplicate := seen[current]; duplicate {
				return errors.New("observation view: replacement cycle")
			}
			seen[current] = struct{}{}
		}
	}
	return nil
}

func stableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
