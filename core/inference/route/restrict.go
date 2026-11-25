package route

import (
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
// A named model is used as given, once the restriction — when there is one — allows it
// and some target can serve it for the modes the call needs. A request that names none
// is answered from the authority: the grant's list in order when the session holds one,
// and otherwise the fleet's own line-up in declaration order, because "every model the
// process serves" is a list too — just one the process wrote.
func (r *Router) resolve(operation string, requested model.ModelRef, modes []model.Mode, providers []string) (model.ModelRef, error) {
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
		if len(r.allow) > 0 {
			return model.ModelRef{}, denied("model.not_granted",
				"model %s is not granted (this session may call %s)", modelName(requested), allowList(r.allow))
		}
		if !r.canServe(requested, modes) {
			return model.ModelRef{}, denied("model.no_candidate",
				"no target serves %s for %s", modelName(requested), modeList(modes))
		}
		return requested, nil
	}

	if len(r.allow) > 0 {
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

	// Unrestricted: choose from what the fleet declares, in declaration order.
	for _, target := range r.targets {
		if target.Provider.Catalog == nil {
			continue
		}
		for _, descriptor := range target.Provider.Catalog.Models() {
			if len(providers) > 0 && !slices.Contains(providers, descriptor.ID.Provider) {
				continue
			}
			ref := model.ModelRef{ID: descriptor.ID}
			if r.canServe(ref, modes) {
				return ref, nil
			}
		}
	}
	return model.ModelRef{}, denied("model.no_candidate",
		"no model this process serves declares %s", modeList(modes))
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
