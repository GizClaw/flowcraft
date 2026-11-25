package route

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
)

// acceptContent drops the targets whose declared inputs cannot carry the request.
//
// `ModelCapabilities.Inputs` is a hard declaration: a model that says it takes text and
// nothing else must not be handed an image and left to fail on the wire, where the error
// reads like a provider fault. An empty list is *undeclared*, not "nothing is allowed" —
// that is the difference that keeps every existing deployment working.
//
// It is done here, in the route, because this is the only layer that holds the catalog and
// therefore the only one that can answer "does any target carry this request?"; a pipeline
// sees one provider and would report a provider failure for a fleet-wide mismatch.
func acceptContent(targets []Target, content message.Content) ([]Target, error) {
	if len(targets) == 0 {
		return targets, nil
	}
	needed := partKinds(content)
	if len(needed) == 0 {
		return targets, nil
	}
	kept := make([]Target, 0, len(targets))
	for _, target := range targets {
		if coversInputs(declaredInputs(target), needed) {
			kept = append(kept, target)
		}
	}
	if len(kept) > 0 {
		return kept, nil
	}
	// Every candidate declared an input list that excludes this request. Naming the part
	// and the model is what makes the failure actionable: "this deployment has no model
	// that reads audio" is a configuration fact, not a provider error.
	return nil, inference.NewError(inference.UnsupportedFeature, "",
		errUnsupportedInput{model: targets[0].Name, part: string(needed[0])})
}

// partKinds lists the distinct content kinds a request carries.
func partKinds(content message.Content) []message.PartKind {
	seen := make(map[message.PartKind]bool, len(content.Parts))
	out := make([]message.PartKind, 0, len(content.Parts))
	for _, part := range content.Parts {
		kind := part.Kind()
		if seen[kind] {
			continue
		}
		seen[kind] = true
		out = append(out, kind)
	}
	return out
}

// coversInputs reports whether a declared input list accepts every needed kind. An empty
// declaration covers everything: it asserts nothing, and a declaration that asserts nothing
// must not filter.
func coversInputs(declared []message.PartKind, needed []message.PartKind) bool {
	if len(declared) == 0 {
		return true
	}
	allowed := make(map[message.PartKind]bool, len(declared))
	for _, kind := range declared {
		allowed[kind] = true
	}
	for _, kind := range needed {
		if !allowed[kind] {
			return false
		}
	}
	return true
}

// declaredInputs is what one target says it accepts: the catalog entry for the model it
// serves, when the target publishes a catalog at all.
func declaredInputs(target Target) []message.PartKind {
	if target.Provider.Catalog == nil || target.Model.Name == "" {
		return nil
	}
	descriptor, found := target.Provider.Catalog.Describe(model.ModelID{
		Provider: target.Model.Provider,
		Name:     target.Model.Name,
	})
	if !found {
		return nil
	}
	return descriptor.Capabilities.Inputs
}

// errUnsupportedInput is the cause behind a fleet-wide input mismatch. It is a value rather
// than a formatted string so a caller can read the model and the part without parsing text.
type errUnsupportedInput struct {
	model string
	part  string
}

func (e errUnsupportedInput) Error() string {
	return "route: no target accepts " + e.part + " input (the first candidate was " + e.model + ")"
}

// preferOutputs reorders candidates so a target whose declared outputs cover the request's
// intent comes first, keeping declaration order inside each group.
//
// This is the other half of the capabilities comment — "routing prefers targets whose declared
// outputs cover the request intent" — and it is deliberately the opposite of [acceptContent]:
// `Outputs` is a preference, so nothing is ever dropped and a fleet that declares no outputs at
// all keeps its declaration order. A request that names no output means text.
func preferOutputs(targets []Target, want inference.Output) []Target {
	if len(targets) < 2 {
		return targets
	}
	kind := outputKind(want)
	preferred := make([]Target, 0, len(targets))
	rest := make([]Target, 0, len(targets))
	for _, target := range targets {
		if coversOutputs(declaredOutputs(target), kind) {
			preferred = append(preferred, target)
			continue
		}
		rest = append(rest, target)
	}
	return append(preferred, rest...)
}

// outputKind maps the request's intent onto the content kind it would produce.
func outputKind(want inference.Output) message.PartKind {
	switch want {
	case inference.OutputImage:
		return message.PartImage
	case inference.OutputAudio:
		return message.PartAudio
	case inference.OutputVideo:
		return message.PartVideo
	default:
		return message.PartText
	}
}

// coversOutputs reports whether a declared output list covers the kind. Undeclared covers
// everything, for the same reason an undeclared input list filters nothing.
func coversOutputs(declared []message.PartKind, want message.PartKind) bool {
	if len(declared) == 0 {
		return true
	}
	for _, kind := range declared {
		if kind == want {
			return true
		}
	}
	return false
}

// declaredOutputs is what one target says it can produce.
func declaredOutputs(target Target) []message.PartKind {
	if target.Provider.Catalog == nil || target.Model.Name == "" {
		return nil
	}
	descriptor, found := target.Provider.Catalog.Describe(model.ModelID{
		Provider: target.Model.Provider,
		Name:     target.Model.Name,
	})
	if !found {
		return nil
	}
	return descriptor.Capabilities.Outputs
}
