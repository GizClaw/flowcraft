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
func acceptContent(targets []Target, want model.ModelID, content message.Content) ([]Target, error) {
	if len(targets) == 0 {
		return targets, nil
	}
	needed := partKinds(content)
	if len(needed) == 0 {
		return targets, nil
	}
	kept := make([]Target, 0, len(targets))
	for _, target := range targets {
		if coversInputs(declaredInputs(target, want), needed) {
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

// declaredInputs is what one target says it accepts for want: the catalog entry for the
// model it serves, when the target publishes a catalog at all.
//
// The id comes from the request when the target declares no model of its own, which is
// what a host-built target looks like: the deployment names the models in its provider's
// catalog, not on each target.
func declaredInputs(target Target, want model.ModelID) []message.PartKind {
	descriptor, found := descriptorFor(target, want)
	if !found {
		return nil
	}
	return descriptor.Capabilities.Inputs
}

// descriptorFor resolves what a target says about want.
func descriptorFor(target Target, want model.ModelID) (model.ModelDescriptor, bool) {
	if target.Provider.Catalog == nil {
		return model.ModelDescriptor{}, false
	}
	if target.Model.Name != "" {
		want = target.Model
	}
	return target.Provider.Catalog.Describe(want)
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

// declaresMode reports whether a target says it can serve mode for want.
//
// A target that publishes no catalog cannot answer, and the historical default applies: a
// model that says nothing is a text generator. That keeps a fleet with no declarations
// working for text and refuses it for anything it never claimed.
func declaresModes(target Target, want model.ModelID, modes []model.Mode) bool {
	for _, mode := range modes {
		if !declaresMode(target, want, mode) {
			return false
		}
	}
	return true
}

// declaresMode reports whether a target says it can serve one mode for want.
func declaresMode(target Target, want model.ModelID, mode model.Mode) bool {
	if descriptor, found := descriptorFor(target, want); found {
		if !descriptor.Capabilities.Declares() {
			// Nothing declared: text is assumed, and the channels that ride on it are
			// not refused — the declaration rule is that an empty one filters nothing.
			return mode == model.ModeText || mode == model.ModeToolCall
		}
		return descriptor.Capabilities.Can(mode)
	}
	// No declaration for this model: the provider's own seams are what it can do. A
	// generator is assumed to produce text — the historical default — while an output
	// modality nobody declared is not assumed, because guessing would hand a model a
	// request it cannot answer.
	//
	// Tool calls are the exception, and it is the declaration rule again: an empty
	// declaration filters nothing. A model that never said whether it has a tool channel
	// is not refused one, because before this mode existed nothing did.
	switch mode {
	case model.ModeText:
		return target.Provider.Generator != nil
	case model.ModeDecide:
		return target.Provider.Decider != nil
	case model.ModeEmbed:
		return target.Provider.Embedder != nil
	case model.ModeToolCall:
		return true
	default:
		return false
	}
}

// outputMode maps the request's intent onto the mode it would produce.
func outputMode(want inference.Output) model.Mode {
	switch want {
	case inference.OutputImage:
		return model.ModeImage
	case inference.OutputAudio:
		return model.ModeAudio
	case inference.OutputVideo:
		return model.ModeVideo
	default:
		return model.ModeText
	}
}
