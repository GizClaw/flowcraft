package config

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/lifecycle"
)

type LifecycleNodeSettings struct {
	ID        string          `yaml:"id"`
	DependsOn []string        `yaml:"depends_on,omitempty"`
	Phase     lifecycle.Phase `yaml:"phase,omitempty"`
	custom    *lifecycle.StepSpec
}

type LifecycleDAGSettings struct {
	Nodes []LifecycleNodeSettings `yaml:"nodes,omitempty"`
}

// LifecycleSettings controls durable Dreaming. It is enabled by default;
// periodic scans are opt-in while source-trigger notifications remain active.
type LifecycleSettings struct {
	Disabled bool                   `yaml:"disabled,omitempty"`
	Periodic bool                   `yaml:"periodic,omitempty"`
	Interval time.Duration          `yaml:"interval,omitempty"`
	LeaseTTL time.Duration          `yaml:"lease_ttl,omitempty"`
	Owner    string                 `yaml:"owner,omitempty"`
	Decay    lifecycle.DecayConfig  `yaml:"decay,omitempty"`
	Forget   lifecycle.ForgetConfig `yaml:"forget,omitempty"`
}

// CustomLifecycleNode binds a programmatic typed lifecycle factory selection.
func CustomLifecycleNode(id string, spec lifecycle.StepSpec, dependsOn ...string) LifecycleNodeSettings {
	return LifecycleNodeSettings{ID: id, DependsOn: append([]string(nil), dependsOn...), custom: &spec}
}
