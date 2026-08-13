package tool

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// Exposure controls whether a tool appears in the model-visible
// Definitions. It is host policy, never part of message.ToolDefinition.
type Exposure string

const (
	// ExposureAlways marks a tool that appears in Definitions every
	// turn. Keep this set small; it is the stable baseline.
	ExposureAlways Exposure = "always"
	// ExposureDirect marks a tool that appears only when it is
	// RequiredByName or has been selected/used recently.
	ExposureDirect Exposure = "direct"
	// ExposureDeferred marks a tool that is hidden until tool_search
	// surfaces it; once selected it stays visible for M rounds.
	ExposureDeferred Exposure = "deferred"
	// ExposureHidden marks a tool that never appears in Definitions
	// (not even via search). RequiredByName can still surface it, and
	// the tool remains callable by exact name at all times.
	ExposureHidden Exposure = "hidden"
)

// Valid reports whether e is one of the four exposure levels.
func (e Exposure) Valid() bool {
	switch e {
	case ExposureAlways, ExposureDirect, ExposureDeferred, ExposureHidden:
		return true
	default:
		return false
	}
}

// rank orders exposures for deterministic pruning: lower ranks are
// evicted last.
func (e Exposure) rank() int {
	switch e {
	case ExposureAlways:
		return 0
	case ExposureDirect:
		return 1
	case ExposureDeferred:
		return 2
	case ExposureHidden:
		return 3
	default:
		return 4
	}
}

// searchable reports whether tool_search may surface the exposure.
func (e Exposure) searchable() bool {
	return e == ExposureDirect || e == ExposureDeferred
}

// Budget caps how many definitions reach the model per turn.
type Budget struct {
	// MaxDefinitions caps the number of definitions. Zero falls back
	// to DefaultBudget.MaxDefinitions.
	MaxDefinitions int `json:"max_definitions,omitempty"`
	// MaxBytes caps the total serialized size of the definitions. Zero
	// falls back to DefaultBudget.MaxBytes.
	MaxBytes int64 `json:"max_bytes,omitempty"`
}

// DefaultBudget is the built-in budget: 32 tools or 16 KiB of
// definitions per turn, whichever is hit first.
var DefaultBudget = Budget{
	MaxDefinitions: 32,
	MaxBytes:       16 * 1024,
}

// Policy is the host-declared injection policy for one assembly.
// Zero values mean "use DefaultPolicy" for every field except
// Exposures, so a partial settings subtree is sufficient.
type Policy struct {
	// Default is the exposure applied to registered tools that have no
	// explicit entry in Exposures. Empty means ExposureDeferred.
	Default Exposure `json:"default,omitempty"`
	// Exposures maps tool names to explicit exposure levels.
	Exposures map[string]Exposure `json:"exposures,omitempty"`
	// SelectedRetention is how many rounds a selected tool stays
	// visible (M). Zero falls back to 5.
	SelectedRetention int `json:"selected_retention,omitempty"`
	// RecentWindow is how many rounds a recently used direct tool stays
	// visible. Zero falls back to 10.
	RecentWindow int `json:"recent_window,omitempty"`
	// Budget caps the visible set. Zero uses DefaultBudget.
	Budget Budget `json:"budget,omitempty"`
	// SearchWithLoad makes tool_search load every deferred source
	// before ranking, so hits are computed over real metadata instead
	// of placeholder declarations. Default is false (lazy search).
	SearchWithLoad bool `json:"search_with_load,omitempty"`
}

// DefaultPolicy returns the recommended policy: everything deferred by
// default, 5 selected rounds, 10 recent rounds, and the default budget.
func DefaultPolicy() Policy {
	return Policy{
		Default:           ExposureDeferred,
		Exposures:         map[string]Exposure{},
		SelectedRetention: 5,
		RecentWindow:      10,
		Budget:            DefaultBudget,
	}
}

// normalizePolicy fills zero fields with DefaultPolicy values so
// settings can express a partial policy.
func normalizePolicy(p Policy) Policy {
	out := p
	if out.Default == "" {
		out.Default = DefaultPolicy().Default
	}
	if out.SelectedRetention <= 0 {
		out.SelectedRetention = DefaultPolicy().SelectedRetention
	}
	if out.RecentWindow <= 0 {
		out.RecentWindow = DefaultPolicy().RecentWindow
	}
	if out.Budget.MaxDefinitions <= 0 {
		out.Budget.MaxDefinitions = DefaultBudget.MaxDefinitions
	}
	if out.Budget.MaxBytes <= 0 {
		out.Budget.MaxBytes = DefaultBudget.MaxBytes
	}
	return out
}

// Validate checks policy invariants. Call after normalizePolicy when
// the policy may come from settings.
func (p Policy) Validate() error {
	if !p.Default.Valid() {
		return errdefs.Validationf(
			"tool: default exposure %q is invalid", p.Default)
	}
	for name, e := range p.Exposures {
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf("tool: exposure map has an empty tool name")
		}
		if !e.Valid() {
			return errdefs.Validationf(
				"tool: exposure %q for tool %q is invalid", e, name)
		}
	}
	return nil
}

func (p Policy) exposureOf(name string) Exposure {
	if e, ok := p.Exposures[name]; ok {
		return e
	}
	return p.Default
}

// definitionBytes approximates the serialized size of a Definition for
// budget pruning without allocating a JSON marshal per turn.
func definitionBytes(d message.ToolDefinition) int64 {
	return int64(len(d.Name) + len(d.Description) + len(d.InputSchema) + 32)
}
