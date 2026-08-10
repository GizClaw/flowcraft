// Package dynamic implements the tool-injection layer described in
// docs/plans/2026-08-10-tool-dynamic-injection.md: an Exposure-driven
// read-side Catalog over a shared tool.Registry, BM25 tool search, and
// lazily loaded proxy tools.
//
// The package deliberately sits on the read side only. Injection rules
// decide what the model sees (Definitions), never what can be called:
// the Executor keeps dispatching against the full Registry, and access
// control stays in the Approval middleware.
package dynamic

import (
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// Exposure controls whether a tool appears in the model-visible
// Definitions. It is host policy, never part of message.Definition.
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
	// (not even via search). RequiredByName can still surface it,
	// matching the legacy ScopePlatform contract, and the tool remains
	// callable by exact name at all times.
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
	// MaxDefinitions caps the number of definitions. Zero falls back to
	// DefaultBudget.MaxDefinitions.
	MaxDefinitions int
	// MaxBytes caps the total serialized size of the definitions. Zero
	// falls back to DefaultBudget.MaxBytes.
	MaxBytes int64
}

// DefaultBudget is the built-in budget: 32 tools or 16 KiB of
// definitions per turn, whichever is hit first.
var DefaultBudget = Budget{
	MaxDefinitions: 32,
	MaxBytes:       16 * 1024,
}

// Policy is the host-declared injection policy for one catalog.
type Policy struct {
	// Default is the exposure applied to registered tools that have no
	// explicit entry in Exposures. Deferred keeps the baseline small:
	// only Always tools and RequiredByName names appear until search
	// selects more.
	Default Exposure
	// Exposures maps tool names to explicit exposure levels.
	Exposures map[string]Exposure
	// SelectedRetention is how many rounds a selected tool stays
	// visible (M in the plan). Zero falls back to 5.
	SelectedRetention int
	// RecentWindow is how many rounds a recently used direct tool stays
	// visible. Zero falls back to 10.
	RecentWindow int
	// Budget caps the visible set. Zero uses DefaultBudget.
	Budget Budget
}

// Validate checks policy invariants. An unset Default is a validation
// error: silently choosing a default would hide or expose every tool
// based on a typo.
func (p Policy) Validate() error {
	if !p.Default.Valid() {
		return errdefs.Validationf(
			"dynamic: default exposure %q is invalid", p.Default)
	}
	for name, e := range p.Exposures {
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf("dynamic: exposure map has an empty tool name")
		}
		if !e.Valid() {
			return errdefs.Validationf(
				"dynamic: exposure %q for tool %q is invalid", e, name)
		}
	}
	return nil
}

// exposureOf resolves the exposure for name, defaulting to p.Default.
func (p Policy) exposureOf(name string) Exposure {
	if e, ok := p.Exposures[name]; ok {
		return e
	}
	return p.Default
}

func (p Policy) selectedRetention() int {
	if p.SelectedRetention <= 0 {
		return 5
	}
	return p.SelectedRetention
}

func (p Policy) recentWindow() int {
	if p.RecentWindow <= 0 {
		return 10
	}
	return p.RecentWindow
}

func (p Policy) budget() Budget {
	b := p.Budget
	if b.MaxDefinitions <= 0 {
		b.MaxDefinitions = DefaultBudget.MaxDefinitions
	}
	if b.MaxBytes <= 0 {
		b.MaxBytes = DefaultBudget.MaxBytes
	}
	return b
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

// definitionBytes approximates the serialized size of a Definition for
// budget pruning without allocating a JSON marshal per turn. The
// approximation is intentionally conservative (adds fixed overhead) and
// deterministic.
func definitionBytes(d message.Definition) int64 {
	return int64(len(d.Name) + len(d.Description) + len(d.InputSchema) + 32)
}

// compactJSON marshals v into a single-line JSON string.
func compactJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
