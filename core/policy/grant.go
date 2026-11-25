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
	// Models lists the callable models. Empty means every model the process
	// serves: which models exist is a property of the process, and the host
	// enforces the set at the call, so "unrestricted" is a meaningful default.
	Models []model.ModelRef `json:"models,omitempty" yaml:"models,omitempty"`
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

// AllowsModel reports whether ref may be called. An empty model set grants every
// model the process serves: what exists at all is a property of the process, the
// same reason an empty tool allow-list means "all of them".
func (g Grant) AllowsModel(ref model.ModelRef) bool { return g.allowsModel(ref) }

func (g Grant) allowsModel(ref model.ModelRef) bool {
	if len(g.Models) == 0 {
		return true
	}
	return slices.ContainsFunc(g.Models, func(granted model.ModelRef) bool {
		return sameModel(granted, ref)
	})
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

// normalizeModels validates and deduplicates a model set while keeping the
// declared order: position is priority, so sorting would reorder a failover
// chain.
func normalizeModels(refs []model.ModelRef) ([]model.ModelRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]model.ModelRef, 0, len(refs))
	for i, ref := range refs {
		if err := ref.Validate(); err != nil {
			return nil, errdefs.Validationf("policy.models[%d]: %w", i, err)
		}
		if slices.Contains(out, ref) {
			continue
		}
		out = append(out, ref)
	}
	return out, nil
}
