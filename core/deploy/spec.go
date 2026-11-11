package deploy

import (
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// CatalogSpec scopes the tool projection a local engine reads.
type CatalogSpec struct {
	// Scope is CatalogScopeRun (default) or CatalogScopeSession.
	Scope string
}

const (
	// CatalogScopeRun gives every Run a fresh view: discoveries persist across
	// the steps of one run and reset on the next.
	CatalogScopeRun = "run"
	// CatalogScopeSession keeps one view per session, so a discovery
	// survives across user turns.
	CatalogScopeSession = "session"
)

// ValidatedScope returns the scope, rejecting anything else.
func (c CatalogSpec) ValidatedScope() (string, error) {
	switch c.Scope {
	case "":
		return CatalogScopeRun, nil
	case CatalogScopeRun, CatalogScopeSession:
		return c.Scope, nil
	}
	return "", errdefs.Validationf(
		"manage: catalog.scope %q is not supported; use %q or %q",
		c.Scope, CatalogScopeRun, CatalogScopeSession)
}

// sortedKeys lists a kind table's names in a stable order.
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Spec is everything needed to deploy one agent: configuration only. Host
// dependencies live in host.Bindings, so a Spec can be compared, logged and
// rebuilt.
type Spec struct {
	// Name identifies the agent. It is unique within a Manager and is what a
	// caller addresses.
	Name string
	// Engine says what runs the agent.
	Engine EngineSpec
	// Protocol says how it is reached. Nil means not exposed.
	Protocol *ProtocolSpec
	// Runtime is the substrate configuration for this agent. Absent for a
	// remote engine, which has no local model, tools or sandbox.
	Runtime runtime.Config
	// Catalog says how the tool projection is scoped. Empty means per run.
	Catalog CatalogSpec
	// IdleTimeout drops a session from memory after this long without use.
	// Zero keeps every session for the agent's lifetime: state lives in the
	// store, so eviction is a cache decision, not a data loss.
	IdleTimeout time.Duration
	// MaxSessions bounds the session table the agent keeps; zero is
	// unbounded. The oldest idle session is evicted first.
	MaxSessions int
	// Delegation is the policy its delegate tools use. Nil means no tools.
	Delegation *DelegationSpec
}

// DelegationSpec is the validated, typed form of the written policy.
type DelegationSpec struct {
	Targets       []string
	Modes         []string
	MaxDepth      int
	MaxConcurrent int
	Timeout       time.Duration
	MaxInputBytes int
}
