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

// rank orders exposures by how much they hide: a narrowing may raise a rank,
// never lower it.
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
		return -1
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

// narrowedTo returns t restricted by req, which must be a subset of t.
//
// "Subset" is per axis: a requested allow-list must sit inside the ceiling's, a
// requested hide-list may only add, an exposure may only hide more, and a budget
// may only shrink. Every violation is a clamp denial, because a request that asks
// for more than its ceiling is a caller bug or an attack, never a preference.
func (t ToolSet) narrowedTo(req ToolSet) (ToolSet, error) {
	if err := req.Validate(); err != nil {
		return ToolSet{}, errdefs.Validationf("policy.tools: requested set is invalid: %w", err)
	}
	out := t

	if req.Allow != nil {
		if t.Allow != nil {
			for _, name := range req.Allow {
				if !slices.Contains(t.Allow, name) {
					return ToolSet{}, deny(CheckVisibility, "tool %q is not in the authority's allow-list", name)
				}
			}
		}
		out.Allow = req.Allow
	}

	for _, name := range req.Hide {
		if t.Allows(name) && !slices.Contains(out.Hide, name) {
			out.Hide = append(slices.Clone(out.Hide), name)
		}
	}
	if len(req.Hide) > 0 {
		out.Hide = sortedUnique(out.Hide)
	}

	if len(t.Exposure) > 0 {
		// Narrowing the allow-list can orphan an inherited exposure: a name the
		// session may no longer call must not keep a presentation override, or the
		// narrowed set would fail its own validation.
		inherited := make(map[string]Exposure, len(t.Exposure))
		for name, level := range t.Exposure {
			if out.Allows(name) {
				inherited[name] = level
			}
		}
		out.Exposure = inherited
		if len(inherited) == 0 {
			out.Exposure = nil
		}
	}
	if len(req.Exposure) > 0 {
		exposure := make(map[string]Exposure, len(out.Exposure)+len(req.Exposure))
		for name, level := range out.Exposure {
			exposure[name] = level
		}
		for name, level := range req.Exposure {
			if !out.Allows(name) {
				return ToolSet{}, deny(CheckVisibility, "tool %q is not callable: its exposure cannot be set", name)
			}
			if level.rank() < t.ExposureOf(name).rank() {
				return ToolSet{}, deny(CheckVisibility,
					"tool %q cannot be exposed more widely than the authority allows (%s -> %s)",
					name, t.ExposureOf(name), level)
			}
			exposure[name] = level
		}
		out.Exposure = exposure
	}

	if req.Budget != (Budget{}) {
		budget, err := t.Budget.narrowedTo(req.Budget)
		if err != nil {
			return ToolSet{}, err
		}
		out.Budget = budget
	}
	return out.Normalized()
}

// narrowedTo keeps the smaller of each cap.
//
// A zero field in the request means "inherit", not "use the default": a request
// that only lowers the definition count must not accidentally reset the byte cap
// to a value larger than its ceiling.
func (b Budget) narrowedTo(req Budget) (Budget, error) {
	ceiling := b.Effective()
	out := b
	if req.MaxDefinitions > 0 {
		if req.MaxDefinitions > ceiling.MaxDefinitions {
			return Budget{}, deny(CheckBudget,
				"max_definitions %d exceeds the authority's %d", req.MaxDefinitions, ceiling.MaxDefinitions)
		}
		out.MaxDefinitions = req.MaxDefinitions
	}
	if req.MaxBytes > 0 {
		if req.MaxBytes > ceiling.MaxBytes {
			return Budget{}, deny(CheckBudget,
				"max_bytes %d exceeds the authority's %d", req.MaxBytes, ceiling.MaxBytes)
		}
		out.MaxBytes = req.MaxBytes
	}
	return out, nil
}

func sortedUnique(names []string) []string {
	if names == nil {
		return nil
	}
	out := slices.Clone(names)
	slices.Sort(out)
	return slices.Compact(out)
}
