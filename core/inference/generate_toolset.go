package inference

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/message"
)

// Toolset is what a model may call on one request: the definitions it was shown
// and the constraint on when it may call them.
//
// It is a value built once and reused. A session's tool list changes rarely —
// when a server connects, when an approval lands — while a request is built
// every turn, so constructing a Toolset validates the definitions and indexes
// their names once, and every later request shares that work.
//
// The zero Toolset means "no tools". Because the definitions and the choice live
// in one value, the cross-field invariant (a forced tool must exist) is enforced
// where the value is built instead of at request validation, and a request
// cannot carry a combination that contradicts itself.
type Toolset struct {
	definitions []message.ToolDefinition
	choice      *ToolChoice
	// names is an index over definitions, built once and shared by every copy
	// of the value. Nothing mutates it after construction, which is what makes
	// copying a Toolset cheap.
	names map[string]struct{}
}

// NewToolset validates the definitions and builds the name index.
// NewToolsetOwned is NewToolset for a caller that already owns the definitions
// and will not mutate them.
//
// A catalog's Definitions returns a fresh slice whose schemas are shared with its
// cache and never change, so cloning them again copies bytes for nothing. Every
// other caller should use NewToolset.
func NewToolsetOwned(definitions []message.ToolDefinition) (Toolset, error) {
	if len(definitions) == 0 {
		return Toolset{}, nil
	}
	names := make(map[string]struct{}, len(definitions))
	for i, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return Toolset{}, fmt.Errorf("tool %d: %w", i, err)
		}
		if _, ok := names[definition.Name]; ok {
			return Toolset{}, fmt.Errorf("duplicate tool %q", definition.Name)
		}
		names[definition.Name] = struct{}{}
	}
	return Toolset{definitions: definitions, names: names}, nil
}

func NewToolset(definitions []message.ToolDefinition) (Toolset, error) {
	if len(definitions) == 0 {
		return Toolset{}, nil
	}
	names := make(map[string]struct{}, len(definitions))
	copied := make([]message.ToolDefinition, len(definitions))
	for i, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return Toolset{}, fmt.Errorf("tool %d: %w", i, err)
		}
		if _, ok := names[definition.Name]; ok {
			return Toolset{}, fmt.Errorf("duplicate tool %q", definition.Name)
		}
		names[definition.Name] = struct{}{}
		copied[i] = definition.Clone()
	}
	return Toolset{definitions: copied, names: names}, nil
}

// Choosing returns a copy of the Toolset constrained by choice. The definitions
// and the name index are shared with the receiver, so this is cheap enough to
// call per step; the choice itself is validated against the definitions.
func (t Toolset) Choosing(choice ToolChoice) (Toolset, error) {
	if err := choice.Validate(); err != nil {
		return Toolset{}, err
	}
	switch choice.Mode {
	case ToolChoiceFunction:
		if !t.Has(choice.Name) {
			return Toolset{}, fmt.Errorf("tool choice names undefined tool %q", choice.Name)
		}
	case ToolChoiceRequired:
		if t.IsZero() {
			return Toolset{}, fmt.Errorf("tool choice %q requires at least one tool", choice.Mode)
		}
	}
	chosen := choice
	t.choice = &chosen
	return t, nil
}

// Definitions returns the definitions the model was shown. The slice is owned by
// the Toolset: read it, do not mutate it, and use Clone for an owned copy.
func (t Toolset) Definitions() []message.ToolDefinition { return t.definitions }

// Choice returns the selection constraint, or nil when the model chooses
// freely. The value is shared with the Toolset: read it, do not mutate it.
func (t Toolset) Choice() *ToolChoice { return t.choice }

// Has reports whether the toolset defines a tool with that name.
func (t Toolset) Has(name string) bool {
	_, ok := t.names[name]
	return ok
}

// Len is the number of definitions.
func (t Toolset) Len() int { return len(t.definitions) }

// IsZero reports whether the toolset carries nothing at all.
func (t Toolset) IsZero() bool { return len(t.definitions) == 0 && t.choice == nil }

// Validate checks a value that was not produced by [NewToolset] — mainly the
// zero value, which is valid, and a value copied through a struct literal.
func (t Toolset) Validate() error {
	if len(t.definitions) == 0 {
		if t.choice != nil && t.choice.Mode == ToolChoiceRequired {
			return fmt.Errorf("tool choice %q requires at least one tool", t.choice.Mode)
		}
		return nil
	}
	if len(t.names) != len(t.definitions) {
		return fmt.Errorf("toolset index is out of sync with its definitions")
	}
	for _, definition := range t.definitions {
		if !t.Has(definition.Name) {
			return fmt.Errorf("toolset index is missing tool %q", definition.Name)
		}
	}
	if t.choice == nil {
		return nil
	}
	_, err := t.Choosing(*t.choice)
	return err
}

// Clone returns a Toolset that owns its definitions.
func (t Toolset) Clone() Toolset {
	if t.definitions == nil && t.choice == nil {
		return Toolset{}
	}
	clone := Toolset{
		choice: cloneToolChoice(t.choice),
	}
	if t.definitions != nil {
		clone.definitions = make([]message.ToolDefinition, len(t.definitions))
		clone.names = make(map[string]struct{}, len(t.definitions))
		for i, definition := range t.definitions {
			clone.definitions[i] = definition.Clone()
			clone.names[definition.Name] = struct{}{}
		}
	}
	return clone
}

// ToolChoiceMode names how a model may pick tools.
type ToolChoiceMode string

const (
	// ToolChoiceAuto lets the model decide whether to call a tool.
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceNone forbids tool calls on this turn.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired forces at least one tool call.
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceFunction forces one named tool.
	ToolChoiceFunction ToolChoiceMode = "function"
)

// ToolChoice constrains tool selection.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	// Name is the forced tool. Required for [ToolChoiceFunction] and
	// rejected otherwise.
	Name string `json:"name,omitempty"`
}

// Validate checks the choice's own invariants. Whether the named tool exists is
// the [Toolset]'s check, because only it knows the definitions.
func (c ToolChoice) Validate() error {
	switch c.Mode {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		if c.Name != "" {
			return fmt.Errorf("tool choice %q does not take a tool name", c.Mode)
		}
		return nil
	case ToolChoiceFunction:
		if c.Name == "" {
			return fmt.Errorf("tool choice %q requires a tool name", c.Mode)
		}
		return nil
	default:
		return fmt.Errorf("unknown tool choice %q", c.Mode)
	}
}

func cloneToolChoice(choice *ToolChoice) *ToolChoice {
	if choice == nil {
		return nil
	}
	clone := *choice
	return &clone
}
