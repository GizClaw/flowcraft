// Package server is craft's half of the plugin wire: it serves each plugin
// process's unix socket with the [protocol.Plugin] and [protocol.Host]
// services and holds the per-connection state a plugin's calls run against.
//
// The primitive set is a catalog, not a specification: [Primitive] is one
// entry, [Catalog] assembles the entries an application contributes plus
// host.about, and nothing here knows which primitive it is holding. An
// application that contributes no primitives still has a host a plugin can
// ask, which is why host.about carries the protocol and never the blessing
// of a fixed family list.
//
// The package deliberately does not import craft/plugin: declarations are
// [protocol] types, and translating a manifest into one is the installer's
// job. That keeps the wire and the install-time types from leaking into
// each other.
package server

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ProtocolVersion is the host primitive protocol version host.about
// reports.
const ProtocolVersion = 1

// GrantSet is the set of grants craft decided on for one plugin: the names
// it holds, whether they came from a manifest's request or were declined.
type GrantSet map[string]bool

// Has reports whether the grant is present.
func (g GrantSet) Has(grant string) bool { return g[grant] }

// List returns the set in name order. It is the shape a set is reported
// in — a summary, a shell entry, a log line — where a map's order would
// make two reports of one decision look like two decisions.
func (g GrantSet) List() []string {
	if len(g) == 0 {
		// No grants is nothing to report, not an empty report: a caller
		// that adds this to a JSON entry gets the field omitted, which is
		// what a plugin holding no grant should look like.
		return nil
	}
	out := make([]string, 0, len(g))
	for grant := range g {
		out = append(out, grant)
	}
	sort.Strings(out)
	return out
}

// Identity is the plugin behind one connection. There is no token: a
// process's grants belong to the socket craft opened for it.
type Identity struct {
	PluginID string
	Grants   GrantSet
}

// Call is one primitive invocation.
type Call struct {
	Identity Identity
	Params   map[string]any
}

// String reads a required string parameter.
func (c Call) String(name string) (string, error) {
	value, ok := c.Params[name].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", errdefs.Validationf("plugin host: parameter %q is required", name)
	}
	return value, nil
}

// Text reads a required string parameter that may be empty: a secret value
// is a string, and "" is a string. Presence and type are what is checked.
func (c Call) Text(name string) (string, error) {
	raw, present := c.Params[name]
	if !present {
		return "", errdefs.Validationf("plugin host: parameter %q is required", name)
	}
	text, ok := raw.(string)
	if !ok {
		return "", errdefs.Validationf(
			"plugin host: parameter %q is %T, want a string", name, raw)
	}
	return text, nil
}

// Object reads an optional parameter as a JSON object. An absent parameter is
// an empty object; a parameter of another shape is refused by name, so a
// plugin sending a string where the catalog documented an object is told
// which field it got wrong.
func (c Call) Object(name string) (map[string]any, error) {
	raw, present := c.Params[name]
	if !present || raw == nil {
		return map[string]any{}, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, errdefs.Validationf(
			"plugin host: parameter %q is %T, want a JSON object", name, raw)
	}
	return object, nil
}

// PrimitiveID joins a resource and an op into the one identifier every
// surface uses: the registry, the charter, the audit log, craft.yaml's
// disabled list and host.about. The wire carries the two halves apart
// because that is the vocabulary; inside craft the joined form is the key.
func PrimitiveID(resource, op string) string { return resource + "." + op }

// SplitPrimitiveID is PrimitiveID's inverse. A malformed id is refused here
// rather than looked up and reported as missing: "secretget" is a typo, and
// a caller that spells a primitive wrong deserves to hear that.
func SplitPrimitiveID(id string) (resource, op string, err error) {
	resource, op, ok := strings.Cut(strings.TrimSpace(id), ".")
	if !ok || !validToken(resource) || !validToken(op) {
		return "", "", errdefs.Validationf(
			"plugin host: primitive %q is not \"resource.op\"", id)
	}
	return resource, op, nil
}

// validToken reports whether name is one half of a primitive id: lowercase
// ASCII letters, digits and underscores, starting with a letter.
func validToken(name string) bool {
	if name == "" {
		return false
	}
	for i, char := range name {
		switch {
		case char >= 'a' && char <= 'z':
		case char == '_' || (char >= '0' && char <= '9' && i > 0):
		default:
			return false
		}
	}
	first := name[0]
	return first >= 'a' && first <= 'z'
}

// Primitive is one host primitive: the resource and op a plugin calls it by,
// the grant it needs, the JSON object it takes, and the handler behind it.
//
// Every primitive has this shape, which is what makes the host surface a
// catalog instead of a set of methods: the application declares them,
// [Catalog] registers them, the registry holds them, the connection serves
// them, and host.about describes them, all without knowing which primitive
// it is holding.
type Primitive struct {
	Resource string
	Op       string
	// Grant is required to call the primitive; empty means always allowed.
	// The name is the application's to choose: it has to be one the plugin
	// store accepts in a manifest, which is how a plugin asks for the
	// primitive at all (see [plugin.Options.Grants]).
	Grant string
	// Description is one line, for host.about.
	Description string
	// Params documents the JSON object the handler takes. host.about reports
	// it verbatim, so a plugin does not have to read the docs to call a
	// primitive it was just told about.
	Params  json.RawMessage
	Handler func(ctx context.Context, call Call) (any, error)
}

// ID is the primitive's "resource.op".
func (p Primitive) ID() string { return PrimitiveID(p.Resource, p.Op) }

// Registry holds the exposed primitives.
type Registry struct {
	mu         sync.RWMutex
	primitives map[string]Primitive
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{primitives: make(map[string]Primitive)}
}

// Register adds one primitive; the resource, the op and the handler are
// required.
func (r *Registry) Register(primitive Primitive) error {
	if !validToken(primitive.Resource) {
		return errdefs.Validationf(
			"plugin host: primitive resource %q is not a name", primitive.Resource)
	}
	if !validToken(primitive.Op) {
		return errdefs.Validationf("plugin host: primitive op %q is not a name",
			primitive.Op)
	}
	if primitive.Handler == nil {
		return errdefs.Validationf(
			"plugin host: primitive %q has no handler", primitive.ID())
	}
	if len(primitive.Params) == 0 {
		primitive.Params = json.RawMessage(paramsNone)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, duplicate := r.primitives[primitive.ID()]; duplicate {
		return errdefs.Conflictf(
			"plugin host: primitive %q already registered", primitive.ID())
	}
	r.primitives[primitive.ID()] = primitive
	return nil
}

// Lookup returns one primitive.
func (r *Registry) Lookup(id string) (Primitive, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	primitive, ok := r.primitives[id]
	return primitive, ok
}

// Primitives returns every primitive in id order.
func (r *Registry) Primitives() []Primitive {
	r.mu.RLock()
	out := make([]Primitive, 0, len(r.primitives))
	for _, primitive := range r.primitives {
		out = append(out, primitive)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Grants returns the distinct grants the primitives require, sorted. An
// application derives its manifest grant vocabulary from this list, which
// is what keeps a primitive's grant and the permission a plugin manifest
// asks for in step.
func Grants(primitives []Primitive) []string {
	seen := make(map[string]bool, len(primitives))
	for _, primitive := range primitives {
		if primitive.Grant != "" {
			seen[primitive.Grant] = true
		}
	}
	out := make([]string, 0, len(seen))
	for grant := range seen {
		out = append(out, grant)
	}
	sort.Strings(out)
	return out
}

// Grants returns [Grants] of the primitives this registry holds.
func (r *Registry) Grants() []string { return Grants(r.Primitives()) }

// About is the host.about answer: what this caller may call, and what it may
// not, with the reason.
type About struct {
	Protocol    int             `json:"protocol"`
	HostVersion string          `json:"host_version,omitempty"`
	Primitives  []PrimitiveInfo `json:"primitives"`
	Denied      []DeniedEntry   `json:"denied,omitempty"`
}

// PrimitiveInfo is one callable primitive in the catalog.
type PrimitiveInfo struct {
	Primitive   string          `json:"primitive"`
	Grant       string          `json:"grant,omitempty"`
	Description string          `json:"description,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
}

// DeniedEntry explains one primitive the caller cannot use.
type DeniedEntry struct {
	Primitive string `json:"primitive"`
	// Reason is no_grant or disabled.
	Reason string `json:"reason"`
}
