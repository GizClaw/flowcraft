// Capabilities
//
// A capability is reached through a typed slot: the package that owns the
// capability declares the slot once, and every reader gets the slot's own type
// back instead of an `any`. Declaring a slot is therefore a local act — no central
// field list, no bitmask, no registration switch — which is what keeps the
// capability carrier small while it stays open to new capabilities.
//
// The split is between the declaration side and the value side:
//
//   - [Slot] is a typed handle. A tool declares it in Needs and reads it out of a
//     [CapView]; a host fills it in a [CapSet].
//   - [Need] is the same slot with its type erased, which is what a declaration
//     list holds: []Need is heterogeneous by nature.
//   - [CapView] is the narrowed half a tool actually receives. A slot the tool did not
//     declare reads as the zero value, which is how "a tool cannot be given a
//     capability it did not ask for" is enforced without copying or nil-ing fields.
//
// A slot is identified by its name, so two declarations with the same name are the
// same slot and must agree on the value's type. Package-level slot variables are the
// intended use; see the naming rule in [NewSlot].
package tool

import "sort"

// Slot is a typed capability handle.
//
// Slots are declared by the package that owns the capability:
//
//	// tool-builtin/files
//	var treeSlot = NewSlot[*fsx.Tree]("fsx.tree")
//
// The value is a plain comparable struct, so a slot may be a package-level variable,
// a const-like field, or copied freely: identity travels with the name.
type Slot[T any] struct {
	name string
}

// NewSlot declares the capability called name.
//
// The name is the slot's identity and the only thing two declarations share, so it
// must be namespaced by the package that owns the capability ("fsx.tree",
// "sandbox.process") and unique across the process. Two slots with the same name are
// the same capability: filling one fills both, and reading through the wrong type
// yields the zero value rather than a panic.
func NewSlot[T any](name string) Slot[T] {
	return Slot[T]{name: name}
}

// Name returns the slot's identity.
func (s Slot[T]) Name() string { return s.name }

// SlotName implements [Need].
func (s Slot[T]) SlotName() string { return s.name }

// Provide fills the slot in set with v. It is the host's side of the contract: only
// the layer that assembled the capability may call it.
func (s Slot[T]) Provide(set *CapSet, v T) {
	if set == nil {
		return
	}
	set.values[s.name] = v
}

// Get reads the slot out of view. It reports false when the slot is absent or was
// not among the needs the view was narrowed to.
func (s Slot[T]) Get(view CapView) (T, bool) {
	raw, ok := view.values[s.name]
	if !ok || !view.allows(s.name) {
		var zero T
		return zero, false
	}
	typed, ok := raw.(T)
	if !ok {
		var zero T
		return zero, false
	}
	return typed, true
}

// Need is a slot with its type erased, which is what a tool's declaration list
// holds. Every [Slot] is a Need; callers never implement it.
type Need interface {
	// SlotName is the declared slot's identity.
	SlotName() string
}

// Set is the capabilities a host assembled for one session. It is a builder: the
// host fills it, hands out a [CapView], and stops touching it.
type CapSet struct {
	values map[string]any
}

// NewCapSet returns an empty set.
func NewCapSet() *CapSet { return &CapSet{values: make(map[string]any)} }

// Has reports whether the set carries need.
func (s *CapSet) Has(need Need) bool {
	if s == nil || need == nil {
		return false
	}
	_, ok := s.values[need.SlotName()]
	return ok
}

// Missing returns the names of the declared needs this set cannot satisfy, sorted so
// a failed assembly reports the same list every run.
//
// It is the construction-time gate: a host validates a tool's Needs against the set
// it built, so a process that cannot serve a tool says so once, at assembly, instead
// of at the first call that happens to reach the missing capability.
func (s *CapSet) Missing(needs ...Need) []string {
	var missing []string
	for _, need := range needs {
		if need == nil || s.Has(need) {
			continue
		}
		missing = append(missing, need.SlotName())
	}
	sort.Strings(missing)
	return missing
}

// CapView returns the unrestricted view over the set.
func (s *CapSet) View() CapView {
	if s == nil {
		return CapView{}
	}
	return CapView{values: s.values}
}

// Allow is one tool's declared needs, precomputed.
//
// A tool's Needs do not change between calls, so the set is built once (at
// registration) and reused: narrowing a view to an Allow is a field assignment, which
// is what keeps the capability carrier free of per-call allocation.
type Allow struct {
	slots map[string]struct{}
}

// AllowOf builds the allowance for a declaration list. It always returns a non-nil
// slot set, so "declared nothing" stays distinguishable from "not narrowed".
func AllowOf(needs ...Need) Allow {
	slots := make(map[string]struct{}, len(needs))
	for _, need := range needs {
		if need != nil {
			slots[need.SlotName()] = struct{}{}
		}
	}
	return Allow{slots: slots}
}

// Len reports how many slots the allowance declares.
func (a Allow) Len() int { return len(a.slots) }

// Has reports whether need is declared.
func (a Allow) Has(need Need) bool {
	if need == nil {
		return false
	}
	_, ok := a.slots[need.SlotName()]
	return ok
}

// noSlots is the fail-closed allowance: a CapView narrowed by a zero Allow reads
// nothing. A missing set must never read as "everything".
var noSlots = map[string]struct{}{}

// CapCapView is the capability half of a tool call: the values, plus the slots this tool
// declared. The zero value carries nothing and reads every slot as absent.
type CapView struct {
	values map[string]any
	allow  map[string]struct{}
}

// With returns the view restricted to a precomputed allowance.
//
// It is the per-call path: a tool's allowance is fixed, so dispatch narrows by
// assignment and no call allocates. A zero Allow reads nothing rather than
// everything — an allowance that failed to build must not widen authority.
func (v CapView) With(a Allow) CapView {
	if v.values == nil {
		return CapView{}
	}
	slots := a.slots
	if slots == nil {
		slots = noSlots
	}
	return CapView{values: v.values, allow: slots}
}

// Narrow returns the view restricted to needs, building the allowance on the spot.
// It is for hosts and tests; a dispatcher precomputes [AllowOf] instead.
func (v CapView) Narrow(needs ...Need) CapView { return v.With(AllowOf(needs...)) }

// Len reports how many capabilities the view can read.
func (v CapView) Len() int {
	if v.values == nil {
		return 0
	}
	if v.allow == nil {
		return len(v.values)
	}
	counted := 0
	for name := range v.values {
		if _, ok := v.allow[name]; ok {
			counted++
		}
	}
	return counted
}

// allows reports whether name is among the declared needs. A nil allow set is the
// unrestricted view a host hands to a caller that asked for none — [Set.View].
func (v CapView) allows(name string) bool {
	if v.allow == nil {
		return true
	}
	_, ok := v.allow[name]
	return ok
}
