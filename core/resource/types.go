package resource

import (
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Kind is the stable identifier of a resource type, e.g. "event.Bus",
// "workspace.Registry", "sandbox.Registry". Kinds live in one global
// string space and are owned by the resource module that implements
// them.
type Kind string

// Ref addresses a whole resource ("workspaces") or one item exported
// by a container resource ("workspaces/fs", "boxes/coding"). A ref has
// at most one slash and never leads or trails with one.
type Ref string

// Split returns the resource name and, when present, the item name.
// ok is false when the ref is empty or malformed.
func (r Ref) Split() (resource, item string, ok bool) {
	text := strings.TrimSpace(string(r))
	if text == "" {
		return "", "", false
	}
	if i := strings.IndexByte(text, '/'); i >= 0 {
		if i == 0 || i == len(text)-1 {
			return "", "", false
		}
		if strings.Contains(text[i+1:], "/") {
			return "", "", false
		}
		return text[:i], text[i+1:], true
	}
	return text, "", true
}

// Validate checks that r is "resource" or "resource/item".
func (r Ref) Validate() error {
	res, _, ok := r.Split()
	if !ok || res == "" {
		return errdefs.Validationf(
			"resource ref %q: must be \"resource\" or \"resource/item\"", r)
	}
	return nil
}

// ResourceName returns the resource part of the ref. The result is
// undefined for malformed refs; validate first.
func (r Ref) ResourceName() string {
	res, _, _ := r.Split()
	return res
}

// ItemName returns the item part of the ref and whether the ref
// carries one.
func (r Ref) ItemName() (string, bool) {
	_, item, ok := r.Split()
	return item, ok && item != ""
}

// Deps is one resource's dependency edges: dep name -> resource ref.
type Deps map[string]Ref

// Validate checks every dep name and ref.
func (d Deps) Validate() error {
	for name, ref := range d {
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf("resource deps: dep name is empty")
		}
		if err := ref.Validate(); err != nil {
			return errdefs.Validationf("resource deps[%q]: %v", name, err)
		}
	}
	return nil
}

// Resource is one entry of a deployment document: the kind, the
// implementation selection, the DAG dependencies, and the opaque
// settings subtree owned by the kind's factory.
type Resource struct {
	Kind     Kind            `json:"kind"`
	Impl     string          `json:"impl,omitempty"`
	Deps     Deps            `json:"deps,omitempty"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Validate checks the static invariants of a document entry: a
// non-empty kind, well-formed deps, and valid JSON settings.
func (r Resource) Validate() error {
	if r.Kind == "" {
		return errdefs.Validationf("resource: kind is required")
	}
	if err := r.Deps.Validate(); err != nil {
		return err
	}
	if len(r.Settings) > 0 && !json.Valid(r.Settings) {
		return errdefs.Validationf("resource %s: settings is not valid JSON", r.Kind)
	}
	return nil
}

// Resources is the resource area of a deployment document.
type Resources map[string]Resource

// Validate checks every named resource.
func (rs Resources) Validate() error {
	for name, res := range rs {
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf("resources: resource name is empty")
		}
		if err := res.Validate(); err != nil {
			return errdefs.Validationf("resources[%q]: %v", name, err)
		}
	}
	return nil
}
