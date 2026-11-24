package policy

import (
	"slices"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/model"
)

// Grant is the permission value: the ceiling an agent may reach, and — after
// [Grant.Narrow] — the facts of one session.
//
// Both uses share one type on purpose. Two types with the same fields would mean
// two validators, two digests and two renderings of the same facts, and the first
// thing two representations do is disagree; the only real difference is "is this
// the ceiling or the result", which the call site already knows.
//
// A validated Grant is immutable. [Grant.Normalized] returns independent copies
// of the slices a narrowing could touch, so a caller mutating its own
// configuration after the fact cannot widen a running session.
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
	// Models lists the callable models. Empty means none: a model call spends
	// money and names a resource with no "everything" spelling.
	Models []model.ModelRef `json:"models,omitempty" yaml:"models,omitempty"`
	// Tools is which tools the session may see and call.
	Tools ToolSet `json:"tools,omitempty" yaml:"tools,omitempty"`
	// Delegation is the authority to hand work to another agent. Nil means
	// none.
	Delegation *Delegation `json:"delegation,omitempty" yaml:"delegation,omitempty"`
	// Limits are the session-table decisions. They are parsed with the grant but
	// they are not permissions: [Grant.Narrow] clears them and the digest
	// ignores them.
	Limits Limits `json:"limits,omitempty" yaml:"limits,omitempty"`
}

// Normalized returns the canonical form and rejects a grant that cannot be
// enforced. It is the load-time gate: a grant that reaches [Grant.Narrow] has
// already passed here.
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
	if err := g.Limits.Validate(); err != nil {
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

// Narrow produces a session's grant from an agent's ceiling plus a caller's
// request. It only ever narrows: every field is compared against the ceiling and
// refused when it exceeds it, and a request that cannot be honoured is an error
// rather than a silently truncated grant — a permission that quietly lost a field
// is indistinguishable from one that never had it.
//
// The request cannot reach files, network, sandbox or delegation: those are
// decided by the host, which is the only layer that knows where the session
// actually runs.
func (g Grant) Narrow(req Request) (Grant, error) {
	base, err := g.Normalized()
	if err != nil {
		return Grant{}, errdefs.Validationf("policy: the grant being narrowed is invalid: %w", err)
	}

	out := base
	// Limits belong to the agent's session table; a session does not limit
	// itself.
	out.Limits = Limits{}

	if len(req.Models) > 0 {
		for _, ref := range req.Models {
			if !base.allowsModel(ref) {
				return Grant{}, deny(CheckModels, "model %s is not granted", modelName(ref))
			}
		}
		models, err := normalizeModels(req.Models)
		if err != nil {
			return Grant{}, err
		}
		out.Models = models
	}

	if req.Tools != nil {
		tools, err := base.Tools.narrowedTo(*req.Tools)
		if err != nil {
			return Grant{}, err
		}
		out.Tools = tools
	}

	if req.Budget != nil {
		budget, err := base.Tools.Budget.narrowedTo(*req.Budget)
		if err != nil {
			return Grant{}, err
		}
		out.Tools.Budget = budget
	}

	return out.Normalized()
}

// AllowsModel reports whether ref may be called. An empty model set grants none.
func (g Grant) AllowsModel(ref model.ModelRef) bool { return g.allowsModel(ref) }

func (g Grant) allowsModel(ref model.ModelRef) bool {
	return len(g.Models) > 0 && slices.Contains(g.Models, ref)
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

func modelName(ref model.ModelRef) string {
	name := ref.ID.Provider + ":" + ref.ID.Name
	if ref.Profile != "" {
		name = ref.Profile + "/" + name
	}
	return name
}
