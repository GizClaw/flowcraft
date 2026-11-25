package route

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
)

// Allow is one model a restricted router may serve, with the modes it may serve it
// for.
//
// Empty Modes mean every mode the model declares: a grant that lists a model without
// narrowing it permits whatever the provider says the model can do.
type Allow struct {
	// Model is the permitted model identity.
	Model model.ModelRef
	// Modes restricts what the model may be asked for.
	Modes []model.Mode
}

// permits reports whether this entry allows every mode a request needs.
func (a Allow) permits(modes []model.Mode) bool {
	if len(a.Modes) == 0 {
		return true
	}
	for _, mode := range modes {
		if !slices.Contains(a.Modes, mode) {
			return false
		}
	}
	return true
}

// Restrict returns a router that only serves the listed models, in the listed
// order.
//
// It shares targets and policy with the receiver — construction is a copy — so a
// session's restriction costs one slice, not a rebuilt fleet. An empty list is "no
// restriction", which is what a router built by a host that has no authority to
// impose keeps; a nil receiver is returned as is.
//
// The list is the only way a request names a model outside it: [Router.Generate] and
// [Router.Decide] refuse one, and choose from the list when the request names none.
func (r *Router) Restrict(allow []Allow) *Router {
	if r == nil {
		return nil
	}
	out := *r
	out.allow = nil
	if len(allow) > 0 {
		out.allow = make([]Allow, len(allow))
		for i, entry := range allow {
			out.allow[i] = Allow{Model: entry.Model, Modes: slices.Clone(entry.Modes)}
		}
	}
	return &out
}

// resolve decides which model one call asks for.
//
// With no restriction the request's own model is used unchanged, which is what an
// unrestricted router has always done. With one, a named model must be listed (and
// the mode it is being used for must be permitted), and a request that names none is
// answered from the list in order: the first entry that permits this mode, that some
// target declares, and — when the request carries provider-specific options — that
// belongs to one of those providers.
func (r *Router) resolve(operation string, requested model.ModelRef, modes []model.Mode, providers []string) (model.ModelRef, error) {
	if len(r.allow) == 0 {
		if requested.ID.Name == "" {
			// Nothing to choose from and nothing named: the configuration is incomplete,
			// and saying so beats sending an empty model to a provider that will report a
			// missing one in its own vocabulary.
			err := inference.NewError(inference.InvalidRequest, inference.FieldModel,
				errors.New("route: no model was named and this router has no list to choose from"))
			err.Detail = "model.not_named"
			return model.ModelRef{}, err
		}
		return requested, nil
	}
	if requested.ID.Name != "" {
		for _, entry := range r.allow {
			if !sameModelRef(entry.Model, requested) {
				continue
			}
			if !entry.permits(modes) {
				return model.ModelRef{}, denied("model.mode_not_permitted",
					"model %s may not be used for %s (this session permits %s)",
					modelName(requested), modeList(modes), modeList(entry.Modes))
			}
			return requested, nil
		}
		return model.ModelRef{}, denied("model.not_granted",
			"model %s is not granted (this session may call %s)", modelName(requested), allowList(r.allow))
	}

	for _, entry := range r.allow {
		if !entry.permits(modes) {
			continue
		}
		if len(providers) > 0 && !slices.Contains(providers, entry.Model.ID.Provider) {
			continue
		}
		if !r.canServe(entry.Model, modes) {
			continue
		}
		return entry.Model, nil
	}
	return model.ModelRef{}, denied("model.no_candidate",
		"no granted model serves %s (this session may call %s)", modeList(modes), allowList(r.allow))
}

// allowsModelID reports whether the restriction lists this model id.
//
// It is what the published catalog enumerates: a profile narrows a target, not a
// descriptor, so the id is the granularity a catalog can answer at.
func (r *Router) allowsModelID(id model.ModelID) bool {
	if len(r.allow) == 0 {
		return true
	}
	for _, entry := range r.allow {
		if entry.Model.ID == id {
			return true
		}
	}
	return false
}

// canServe reports whether some target declares ref for mode.
func (r *Router) canServe(ref model.ModelRef, modes []model.Mode) bool {
	for _, target := range r.targets {
		if !servesModel(target, ref.ID) || !servesProfile(target.Profile, ref.Profile) {
			continue
		}
		if declaresModes(target, ref.ID, modes) {
			return true
		}
	}
	return false
}

// sameModelRef reports whether granted names the model want asks for. A granted ref
// without a profile names the model wherever it is served; one with a profile pins
// the deployment.
func sameModelRef(granted, want model.ModelRef) bool {
	if granted.ID != want.ID {
		return false
	}
	return granted.Profile == "" || granted.Profile == want.Profile
}

// denied builds the classification every restriction refusal carries.
//
// The detail is a stable label — what a dashboard groups by — and the rendered sentence
// is the cause, which a caller reaches with errors.Unwrap and a log line shows in full.
// The other route refusals keep their message in the cause the same way.
func denied(detail, format string, args ...any) error {
	err := inference.NewError(inference.PolicyDenied, inference.FieldModel,
		fmt.Errorf("route: "+format, args...))
	err.Detail = detail
	return err
}

// allowList renders the permitted models for a model-facing message.
func allowList(allow []Allow) string {
	names := make([]string, 0, len(allow))
	for _, entry := range allow {
		names = append(names, modelName(entry.Model))
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// modeList renders a mode set for a model-facing message.
func modeList(modes []model.Mode) string {
	if len(modes) == 0 {
		return "every mode its provider declares"
	}
	names := make([]string, 0, len(modes))
	for _, mode := range modes {
		names = append(names, string(mode))
	}
	return strings.Join(names, ", ")
}

// modelName renders one ref the way a document writes it.
func modelName(ref model.ModelRef) string {
	name := ref.ID.Provider + "/" + ref.ID.Name
	if ref.Profile != "" {
		name = ref.Profile + "@" + name
	}
	return name
}
