package policy

import (
	"slices"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/model"
)

// Grant is the permission value: what one session may do, stated by whoever opens
// it.
//
// It is a value rather than a gate. The host reads it to build the boundary, and a
// tool reads it to refuse a call with a message the model can correct itself with.
// There is no second representation, because two of them would mean two validators,
// two digests and two renderings of the same facts — and the first thing two
// representations do is disagree.
//
// A validated Grant is immutable. [Grant.Normalized] returns independent copies of
// the slices it owns, so a caller mutating its own configuration after the fact
// cannot change a running session.
type Grant struct {
	// Sandbox names the isolation template the runtime offers. Empty means the
	// runtime's default; a runtime with no templates and a named sandbox is a
	// session-open failure, not a silent fallback.
	Sandbox string `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
	// Files is the file permission.
	Files Files `json:"files,omitempty" yaml:"files,omitempty"`
	// Net is the egress posture.
	Net NetPolicy `json:"net,omitempty" yaml:"net,omitempty"`
	// Exec is the command-execution policy.
	Exec ExecPolicy `json:"exec,omitempty" yaml:"exec,omitempty"`
	// Resources caps what a command may consume.
	Resources Resources `json:"resources,omitempty" yaml:"resources,omitempty"`
	// Models lists the callable models, in priority order: the first entry a mode can
	// use is the one a call that names no model gets. Empty means every model the
	// process serves.
	Models []ModelGrant `json:"models,omitempty" yaml:"models,omitempty"`
	// Tools is which tools the session may see and call.
	Tools ToolSet `json:"tools,omitempty" yaml:"tools,omitempty"`
	// Delegation is the authority to hand work to another agent. Nil means
	// none.
	Delegation *Delegation `json:"delegation,omitempty" yaml:"delegation,omitempty"`
}

// Normalized returns the canonical form and rejects a grant that cannot be
// enforced. It runs where the grant enters — at open for a session, at build for an
// approval — so everything downstream may assume it passed.
func (g Grant) Normalized() (Grant, error) {
	files, err := g.Files.Normalized()
	if err != nil {
		return Grant{}, err
	}
	tools, err := g.Tools.Normalized()
	if err != nil {
		return Grant{}, err
	}
	net, err := g.Net.Normalized()
	if err != nil {
		return Grant{}, err
	}
	if err := g.Exec.Validate(); err != nil {
		return Grant{}, err
	}
	if err := g.Resources.Validate(); err != nil {
		return Grant{}, err
	}
	if g.Delegation != nil {
		if err := g.Delegation.Validate(); err != nil {
			return Grant{}, err
		}
	}
	models, err := normalizeModels(g.Models)
	if err != nil {
		return Grant{}, err
	}

	out := g
	out.Files = files
	out.Net = net
	out.Tools = tools
	out.Models = models
	out.Delegation = g.Delegation.Clone()
	return out, nil
}

// Validate reports whether the grant can be enforced.
func (g Grant) Validate() error {
	_, err := g.Normalized()
	return err
}

// ModelGrant is one model a session may call, and the modes it may call it for.
//
// It embeds the ref instead of nesting it, so a document keeps writing
// `models: [{id: {...}, profile: ...}]` and adds `modes` only where it narrows.
// Empty Modes mean every mode the model declares — a grant that lists a model without
// qualifying it permits what the provider says the model can do.
type ModelGrant struct {
	model.ModelRef
	// Modes restricts what the model may be asked for: text, image, audio, video,
	// embed or decide.
	Modes []model.Mode `json:"modes,omitempty" yaml:"modes,omitempty"`
}

// Validate rejects a mode set no model could honour.
func (m ModelGrant) Validate() error {
	if err := m.ModelRef.Validate(); err != nil {
		return err
	}
	seen := make(map[model.Mode]struct{}, len(m.Modes))
	for _, mode := range m.Modes {
		if !mode.Valid() {
			return errdefs.Validationf("mode %q is not one of text, image, audio, video, embed or decide", mode)
		}
		if _, duplicate := seen[mode]; duplicate {
			return errdefs.Validationf("mode %q is declared twice", mode)
		}
		seen[mode] = struct{}{}
	}
	return nil
}

// AllowsModel reports whether ref may be called at all, whichever mode asks. An
// empty model set grants every model the process serves: what exists at all is a
// property of the process, the same reason an empty tool allow-list means "all of
// them".
func (g Grant) AllowsModel(ref model.ModelRef) bool {
	if len(g.Models) == 0 {
		return true
	}
	for _, granted := range g.Models {
		if sameModel(granted.ModelRef, ref) {
			return true
		}
	}
	return false
}

// AllowsModelMode reports whether ref may be called for mode.
//
// An entry without modes permits every mode the model declares, so this answers the
// permission half of the question; whether the model can do it at all is the
// provider's declaration, which the router reads from the catalog.
func (g Grant) AllowsModelMode(ref model.ModelRef, mode model.Mode) bool {
	if len(g.Models) == 0 {
		return true
	}
	for _, granted := range g.Models {
		if !sameModel(granted.ModelRef, ref) {
			continue
		}
		return len(granted.Modes) == 0 || slices.Contains(granted.Modes, mode)
	}
	return false
}

// sameModel reports whether a granted ref names the model want asks for.
//
// A ref without a profile names a model wherever it is served; a ref with one
// pins the deployment. Without this asymmetry a grant could not say "this model,
// whichever account answers" and a caller that named no profile would be refused
// a model the grant actually listed.
func sameModel(granted, want model.ModelRef) bool {
	if granted.ID != want.ID {
		return false
	}
	return granted.Profile == "" || granted.Profile == want.Profile
}

// AllowsTool reports whether name may be called.
func (g Grant) AllowsTool(name string) bool { return g.Tools.Allows(name) }

// AllowsWrite reports whether path may be written. It is the declarative half of
// the file boundary: a backend that cannot enforce the same rule must refuse the
// session rather than accept calls this rejects.
func (g Grant) AllowsWrite(path string) bool {
	switch {
	case g.Files.ReadOnly:
		return false
	case len(g.Files.Write) == 0:
		return true
	default:
		return coveredBy(g.Files.Write, path)
	}
}

// AllowsRead reports whether path may be read.
func (g Grant) AllowsRead(path string) bool {
	return len(g.Files.Read) == 0 || coveredBy(g.Files.Read, path)
}

// normalizeModels validates and deduplicates a model set while keeping the declared
// order: position is priority, so sorting would reorder the list a call without a
// model name chooses from.
//
// A model listed twice keeps its first position and the **intersection** of its mode
// sets, because a repeated entry can only narrow: empty means every mode, so
// intersecting with it is the other set. A caller that repeated a model to widen it
// wrote a list whose meaning depended on which entry was read first.
func normalizeModels(grants []ModelGrant) ([]ModelGrant, error) {
	if len(grants) == 0 {
		return nil, nil
	}
	out := make([]ModelGrant, 0, len(grants))
	for i, grant := range grants {
		if err := grant.Validate(); err != nil {
			return nil, errdefs.Validationf("policy.models[%d]: %w", i, err)
		}
		merged := false
		for j, existing := range out {
			if !sameModel(existing.ModelRef, grant.ModelRef) {
				continue
			}
			out[j].Modes = intersectModes(existing.Modes, grant.Modes)
			merged = true
			break
		}
		if merged {
			continue
		}
		out = append(out, ModelGrant{
			ModelRef: grant.ModelRef,
			Modes:    slices.Clone(grant.Modes),
		})
	}
	return out, nil
}

// intersectModes keeps the modes both sides name, treating an empty set as "every
// mode" so a plain entry never narrows or widens a qualified one by accident.
func intersectModes(left, right []model.Mode) []model.Mode {
	switch {
	case len(left) == 0:
		return slices.Clone(right)
	case len(right) == 0:
		return slices.Clone(left)
	}
	allowed := make(map[model.Mode]struct{}, len(right))
	for _, mode := range right {
		allowed[mode] = struct{}{}
	}
	out := make([]model.Mode, 0, len(left))
	for _, mode := range left {
		if _, ok := allowed[mode]; ok {
			out = append(out, mode)
		}
	}
	return out
}
