package bindings

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// messagePrefix opens every error this package writes about a script
// surface. A merge strips it off a part's failure before naming the part,
// so a merge nested in another prints it once instead of once per level.
const messagePrefix = "script bindings: "

// Part is one provider's contribution to a merged surface: the provider
// itself, and the label the merge reports it under.
type Part struct {
	// Owner labels the contributor in the errors the merge reports — a
	// deployment dep key, say, written the way the message should read
	// ("dep \"unit.tokens\""). It is printed verbatim, in front of a
	// failure the contributor caused and beside the owners that bound the
	// same global; the unlabeled side of a conflict reads "an unlabeled
	// part". An empty Owner leaves a part's own errors unwrapped and a
	// conflict between unlabeled parts unattributed.
	Owner string

	// Provider supplies the contribution. A nil Provider contributes
	// nothing.
	Provider Provider
}

// Merge composes parts into one provider: ordinary bindings run part by
// part in order, then each part's late phase in the same order. The
// result is both a [Provider] and a [LateProvider], so [Assemble] treats
// it like any other surface.
//
// Unlike [Assemble], which sees one finished list, a merge knows which
// part bound what: a duplicate global is reported with the parts that
// bound it, and a part's own failure keeps its classification while
// gaining the part's owner. A merge also rejects the names [Assemble]
// would reject — an empty name, or one no script could reference — so a
// composed surface fails at the offending part instead of at
// installation.
//
// [Chain] is a merge of unlabeled parts; a caller that can attribute a
// conflict to something more useful than a position passes owners. Merge
// keeps a copy of parts, and like any provider the surface it returns is
// built once and shared across executions, so configure it before use.
func Merge(parts ...Part) Provider {
	return merged{parts: append([]Part(nil), parts...)}
}

// merged is the composed surface: the parts it was built from, in binding
// order. They live in a field of a struct value rather than being the
// slice itself, so a merge has no pointer identity of its own — what a
// merge binds is its parts, not where they are stored.
type merged struct {
	parts []Part
}

// preBound marks a global that was bound before the late phase ran: the
// merge can report that the name was taken, not which part took it, since
// the ordinary phase that bound it has already returned.
const preBound = -1

// Bind implements [Provider].
func (m merged) Bind(inv Invocation) ([]Binding, error) {
	out := make([]Binding, 0)
	owner := make(map[string]int, len(m.parts)) // global name -> part index
	for index, part := range m.parts {
		list, err := part.bind(inv)
		if err != nil {
			return nil, err
		}
		for _, b := range list {
			if err := bindingNameError(b.Name); err != nil {
				return nil, part.attribute(err)
			}
			if first, dup := owner[b.Name]; dup {
				return nil, mergeDuplicatef(b.Name, m.parts[first].Owner, part.Owner)
			}
			owner[b.Name] = index
			out = append(out, b)
		}
	}
	return out, nil
}

// BindLate implements [LateProvider]: every part with a late phase runs
// it in order, against the environment the ordinary bindings built.
// [Assemble] installs the returned bindings after this call, so a late
// binding does not see its siblings — the same contract as [Chain].
//
// The environment is also how the merge detects a late collision with an
// earlier name: it reads the globals already installed there. Pass the
// environment [Assemble] installed the ordinary bindings into; an empty
// one leaves the clash to the caller's own duplicate check.
func (m merged) BindLate(inv Invocation, env *agent.ScriptEnv) ([]Binding, error) {
	out := make([]Binding, 0)
	// Global name -> part index, or preBound for a name the environment
	// already carries: this merge's ordinary phase, or an enclosing
	// provider's.
	owner := make(map[string]int, len(m.parts))
	if env != nil {
		for name := range env.Bindings {
			owner[name] = preBound
		}
	}
	for index, part := range m.parts {
		late, ok := part.Provider.(LateProvider)
		if !ok {
			continue
		}
		list, err := part.bindLate(late, inv, env)
		if err != nil {
			return nil, err
		}
		for _, b := range list {
			if err := bindingNameError(b.Name); err != nil {
				return nil, part.attribute(err)
			}
			first, dup := owner[b.Name]
			switch {
			case !dup:
			case first == preBound:
				return nil, mergeLateCollisionf(b.Name, part.Owner)
			default:
				return nil, mergeDuplicatef(b.Name, m.parts[first].Owner, part.Owner)
			}
			owner[b.Name] = index
			out = append(out, b)
		}
	}
	return out, nil
}

// bind runs the part's ordinary phase, attributing a failure to it.
func (p Part) bind(inv Invocation) ([]Binding, error) {
	if err := p.typedNil(); err != nil {
		return nil, err
	}
	if p.Provider == nil {
		return nil, nil
	}
	list, err := p.Provider.Bind(inv)
	if err != nil {
		return nil, p.attribute(err)
	}
	return list, nil
}

// bindLate runs the part's late phase, attributing a failure to it.
func (p Part) bindLate(late LateProvider, inv Invocation, env *agent.ScriptEnv) ([]Binding, error) {
	if err := p.typedNil(); err != nil {
		return nil, err
	}
	list, err := late.BindLate(inv, env)
	if err != nil {
		return nil, p.attribute(err)
	}
	return list, nil
}

// typedNil rejects a provider that is a nil pointer or similar: calling
// it would panic rather than fail the execution. The chain resource
// rejects one at build time, so this guards a surface assembled by hand.
func (p Part) typedNil() error {
	if p.Provider == nil || !ptr.IsNil(p.Provider) {
		return nil
	}
	if p.Owner == "" {
		return errdefs.Validationf("%sprovider is a typed nil %T", messagePrefix, p.Provider)
	}
	return errdefs.Validationf(
		"%s%s: provider is a typed nil %T", messagePrefix, p.Owner, p.Provider)
}

// attribute puts the part's owner in front of a failure it caused,
// leaving the error's classification intact. An unlabeled part's error
// travels unchanged, which is what makes [Chain] compose like a plain
// concatenation.
func (p Part) attribute(err error) error {
	if p.Owner == "" || err == nil {
		return err
	}
	return fmt.Errorf("%s%s: %w", messagePrefix, p.Owner, unprefixed{err})
}

// unprefixed renders err without the prefix this package opens its
// messages with, so a merge nested in another names one owner per level
// instead of repeating "script bindings:" on the same line.
type unprefixed struct{ err error }

func (u unprefixed) Error() string { return strings.TrimPrefix(u.err.Error(), messagePrefix) }
func (u unprefixed) Unwrap() error { return u.err }

// mergeDuplicatef reports one global bound twice within a merge. Two
// unlabeled parts cannot be told apart, so that case keeps the wording
// [Assemble] uses for the same collision.
func mergeDuplicatef(name, first, second string) error {
	switch {
	case first == "" && second == "":
		return errdefs.Validationf(
			"script bindings: global %q is bound more than once", name)
	case first == second:
		return errdefs.Validationf(
			"script bindings: global %q is bound twice by %s", name, first)
	default:
		return errdefs.Validationf(
			"script bindings: global %q bound by both %s and %s",
			name, ownerLabel(first), ownerLabel(second))
	}
}

// mergeLateCollisionf reports a late binding that collides with a global
// the environment already carries. A labeled part names itself; an
// unlabeled one can only report the collision.
func mergeLateCollisionf(name, owner string) error {
	if owner == "" {
		return errdefs.Validationf(
			"script bindings: global %q is bound again in the late phase", name)
	}
	return errdefs.Validationf(
		"script bindings: global %q bound by %s was already bound before the late phase",
		name, owner)
}

// ownerLabel renders an owner for a conflict message: an unlabeled part
// has no name to print.
func ownerLabel(owner string) string {
	if owner == "" {
		return "an unlabeled part"
	}
	return owner
}
