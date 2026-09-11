package model

import (
	"fmt"
	"time"
)

type ModelStatus string

const (
	ModelStatusActive     ModelStatus = "active"
	ModelStatusDeprecated ModelStatus = "deprecated"
	ModelStatusRetired    ModelStatus = "retired"
)

// ModelLifecycle is discovery metadata, not an execution capability claim.
// An empty value means active.
type ModelLifecycle struct {
	Status      ModelStatus `json:"status,omitempty"`
	RetiresAt   *time.Time  `json:"retires_at,omitempty"`
	Replacement *ModelID    `json:"replacement,omitempty"`
	Notes       string      `json:"notes,omitempty"`
}

func (l ModelLifecycle) Clone() ModelLifecycle {
	clone := l
	clone.RetiresAt = ClonePointer(l.RetiresAt)
	clone.Replacement = ClonePointer(l.Replacement)
	return clone
}

func (l ModelLifecycle) ValidateFor(model ModelID) error {
	status := l.Status
	if status == "" {
		status = ModelStatusActive
	}
	switch status {
	case ModelStatusActive:
		if l.RetiresAt != nil || l.Replacement != nil || l.Notes != "" {
			return fmt.Errorf("active model cannot carry retirement metadata")
		}
	case ModelStatusDeprecated, ModelStatusRetired:
	default:
		return fmt.Errorf("unknown model status %q", l.Status)
	}
	if l.RetiresAt != nil && l.RetiresAt.IsZero() {
		return fmt.Errorf("model retirement time must not be zero")
	}
	if l.Replacement != nil {
		if err := l.Replacement.Validate(); err != nil {
			return fmt.Errorf("replacement: %w", err)
		}
		if *l.Replacement == model {
			return fmt.Errorf("replacement must differ from the model")
		}
	}
	return nil
}
