package policy

import (
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Exposure controls whether a tool reaches the model's definition list. It is
// presentation policy: it never changes what a session may *call*.
type Exposure string

const (
	// ExposureAlways is defined every round. Keep this set small; it is the
	// stable baseline.
	ExposureAlways Exposure = "always"
	// ExposureDirect appears only when it is required by name or has been
	// surfaced by discovery.
	ExposureDirect Exposure = "direct"
	// ExposureDeferred is hidden until a search surfaces it.
	ExposureDeferred Exposure = "deferred"
	// ExposureHidden never appears, though an exact call by name still works.
	ExposureHidden Exposure = "hidden"
)

// Valid reports whether e is one of the four levels.
func (e Exposure) Valid() bool {
	switch e {
	case ExposureAlways, ExposureDirect, ExposureDeferred, ExposureHidden:
		return true
	default:
		return false
	}
}

// Budget caps what reaches the model in one round. Zero on either field means the
// built-in default.
type Budget struct {
	// MaxDefinitions caps the number of definitions.
	MaxDefinitions int `json:"max_definitions,omitempty" yaml:"max_definitions,omitempty"`
	// MaxBytes caps the total serialized size of the definitions.
	MaxBytes int64 `json:"max_bytes,omitempty" yaml:"max_bytes,omitempty"`
}

// DefaultBudget is what an unconfigured budget resolves to.
var DefaultBudget = Budget{MaxDefinitions: 32, MaxBytes: 16 << 10}

// Effective fills the zero fields with the built-in default.
func (b Budget) Effective() Budget {
	out := b
	if out.MaxDefinitions <= 0 {
		out.MaxDefinitions = DefaultBudget.MaxDefinitions
	}
	if out.MaxBytes <= 0 {
		out.MaxBytes = DefaultBudget.MaxBytes
	}
	return out
}

// Validate rejects a budget that cannot be honoured.
func (b Budget) Validate() error {
	if b.MaxDefinitions < 0 {
		return errdefs.Validationf("policy.tools.budget: max_definitions must not be negative")
	}
	if b.MaxBytes < 0 {
		return errdefs.Validationf("policy.tools.budget: max_bytes must not be negative")
	}
	return nil
}

// ToolSet is which tools a session may see and call.
//
// An empty Allow list means every tool the process wired — which tools exist is a
// property of the process, so "all of them" is a meaningful default. Hide always
// wins over Allow, and the two lists may not name the same tool: an entry that is
// both allowed and hidden is a configuration whose meaning depends on which check
// runs first.
type ToolSet struct {
	// Allow lists callable tool names. Empty means all.
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	// Hide removes names from the allowed set.
	Hide []string `json:"hide,omitempty" yaml:"hide,omitempty"`
	// Exposure overrides the presentation level of a name. A name not listed
	// is [ExposureAlways].
	Exposure map[string]Exposure `json:"exposure,omitempty" yaml:"exposure,omitempty"`
	// Budget caps what reaches the model each round.
	Budget Budget `json:"budget,omitempty" yaml:"budget,omitempty"`
}

// Allows reports whether name may be called under this set.
func (t ToolSet) Allows(name string) bool {
	if name == "" || slices.Contains(t.Hide, name) {
		return false
	}
	if t.Allow == nil {
		return true
	}
	return slices.Contains(t.Allow, name)
}

// ExposureOf returns the presentation level of name.
func (t ToolSet) ExposureOf(name string) Exposure {
	if level, ok := t.Exposure[name]; ok {
		return level
	}
	return ExposureAlways
}

// EffectiveBudget returns the budget with defaults applied.
func (t ToolSet) EffectiveBudget() Budget { return t.Budget.Effective() }

// IsZero reports whether the set constrains nothing.
func (t ToolSet) IsZero() bool {
	return t.Allow == nil && len(t.Hide) == 0 && len(t.Exposure) == 0 && t.Budget == Budget{}
}

// Normalized returns the canonical form: lists deduplicated and sorted so that
// one permission has one digest.
func (t ToolSet) Normalized() (ToolSet, error) {
	if err := t.Validate(); err != nil {
		return ToolSet{}, err
	}
	out := t
	out.Allow = sortedUnique(t.Allow)
	out.Hide = sortedUnique(t.Hide)
	if len(t.Exposure) > 0 {
		out.Exposure = make(map[string]Exposure, len(t.Exposure))
		for name, level := range t.Exposure {
			out.Exposure[name] = level
		}
	}
	return out, nil
}

// Validate rejects a set whose meaning is ambiguous.
func (t ToolSet) Validate() error {
	for i, name := range t.Allow {
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf("policy.tools.allow[%d] is empty", i)
		}
		if strings.TrimSpace(name) != name {
			return errdefs.Validationf("policy.tools.allow[%d] %q has surrounding whitespace", i, name)
		}
		if slices.Contains(t.Hide, name) {
			return errdefs.Validationf("policy.tools: %q is both allowed and hidden", name)
		}
	}
	for i, name := range t.Hide {
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf("policy.tools.hide[%d] is empty", i)
		}
	}
	for name := range t.Exposure {
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf("policy.tools.exposure: empty tool name")
		}
		if !t.Allows(name) {
			return errdefs.Validationf("policy.tools.exposure: %q is not callable under this set", name)
		}
	}
	for name, level := range t.Exposure {
		if !level.Valid() {
			return errdefs.Validationf("policy.tools.exposure[%s]: unknown level %q", name, level)
		}
	}
	return t.Budget.Validate()
}

func sortedUnique(names []string) []string {
	if names == nil {
		return nil
	}
	out := slices.Clone(names)
	slices.Sort(out)
	return slices.Compact(out)
}
