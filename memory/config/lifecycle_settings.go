package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/lifecycle"
)

type LifecycleNodeSettings struct {
	ID        string          `json:"id"`
	DependsOn []string        `json:"depends_on,omitempty"`
	Phase     lifecycle.Phase `json:"phase,omitempty"`
	custom    *lifecycle.StepSpec
}

type LifecycleDAGSettings struct {
	Nodes []LifecycleNodeSettings `json:"nodes,omitempty"`
}

// LifecycleSettings controls durable Dreaming. It is enabled by default;
// periodic scans are opt-in while source-trigger notifications remain active.
type LifecycleSettings struct {
	Disabled bool                   `json:"disabled,omitempty"`
	Periodic bool                   `json:"periodic,omitempty"`
	Interval Duration               `json:"interval,omitempty"`
	LeaseTTL Duration               `json:"lease_ttl,omitempty"`
	Owner    string                 `json:"owner,omitempty"`
	Decay    lifecycle.DecayConfig  `json:"decay,omitempty"`
	Forget   lifecycle.ForgetConfig `json:"forget,omitempty"`
}

// decayWire mirrors lifecycle.DecayConfig with duration-string decoding
// so LifecycleSettings can decode a JSON subtree strictly without
// changing the imported config type's wire handling.
type decayWire struct {
	Version         string   `json:"version"`
	HalfLife        Duration `json:"half_life"`
	RecencyWeight   float64  `json:"recency_weight"`
	FrequencyWeight float64  `json:"frequency_weight"`
	RelevanceWeight float64  `json:"relevance_weight"`
	FrequencyScale  float64  `json:"frequency_scale"`
}

type lifecycleWire struct {
	Disabled bool                   `json:"disabled,omitempty"`
	Periodic bool                   `json:"periodic,omitempty"`
	Interval Duration               `json:"interval,omitempty"`
	LeaseTTL Duration               `json:"lease_ttl,omitempty"`
	Owner    string                 `json:"owner,omitempty"`
	Decay    decayWire              `json:"decay,omitempty"`
	Forget   lifecycle.ForgetConfig `json:"forget,omitempty"`
}

// UnmarshalJSON decodes lifecycle settings strictly, translating
// duration strings for fields whose types are owned by other packages.
func (s *LifecycleSettings) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire lifecycleWire
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	*s = LifecycleSettings{
		Disabled: wire.Disabled,
		Periodic: wire.Periodic,
		Interval: wire.Interval,
		LeaseTTL: wire.LeaseTTL,
		Owner:    wire.Owner,
		Decay: lifecycle.DecayConfig{
			Version:         wire.Decay.Version,
			HalfLife:        time.Duration(wire.Decay.HalfLife),
			RecencyWeight:   wire.Decay.RecencyWeight,
			FrequencyWeight: wire.Decay.FrequencyWeight,
			RelevanceWeight: wire.Decay.RelevanceWeight,
			FrequencyScale:  wire.Decay.FrequencyScale,
		},
		Forget: wire.Forget,
	}
	return nil
}

// CustomLifecycleNode binds a programmatic typed lifecycle factory selection.
func CustomLifecycleNode(id string, spec lifecycle.StepSpec, dependsOn ...string) LifecycleNodeSettings {
	return LifecycleNodeSettings{ID: id, DependsOn: append([]string(nil), dependsOn...), custom: &spec}
}
