package server

import (
	"context"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// paramsNone is the schema of a primitive that documents no parameters. It
// is the default of [Primitive.Params], so host.about always has something
// to report and a plugin never has to read an empty blob as "no arguments
// allowed".
const paramsNone = `{"type":"object"}`

// Catalog assembles the host surface: one entry per primitive, plus
// host.about, minus the ids the configuration disables.
//
// It is the one place where the primitive set is decided, which is what
// makes host.about's answer complete: a primitive is either served or
// reported as denied, never both and never neither. It is also the only
// check the disabled list gets — the list names primitives of this
// catalog, and the catalog is known here, so a typo fails the build of the
// host instead of reading as "off" while every plugin still sees the
// primitive.
//
// Craft ships no primitives of its own. A host that serves nothing is
// still a host a plugin can ask: host.about answers with the empty catalog
// and the protocol version, and no primitive is reachable that nobody
// registered.
func Catalog(
	primitives []Primitive,
	hostVersion string,
	disabled []string,
) (*Registry, error) {
	registry := NewRegistry()
	known := make(map[string]bool, len(primitives)+1)
	known[PrimitiveID("host", "about")] = true
	for _, primitive := range primitives {
		known[primitive.ID()] = true
	}
	disabledSet := make(map[string]bool, len(disabled))
	for _, id := range disabled {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !known[id] {
			return nil, errdefs.Validationf(
				"plugin host: cannot disable %q: it is not a host primitive", id)
		}
		disabledSet[id] = true
	}
	// host.about is the one primitive that is never optional: it is how a
	// plugin learns which primitives exist and which it is denied, and the
	// list a disabled primitive is reported in is its answer. A
	// configuration that asks to hide it cannot be honored, and quietly
	// ignoring the entry would leave an application believing a primitive
	// it named is not exposed while every plugin still sees it.
	if disabledSet[PrimitiveID("host", "about")] {
		return nil, errdefs.Validationf(
			"plugin host: host.about cannot be disabled: it reports the primitive set")
	}
	denied := make([]DeniedEntry, 0, len(disabledSet))
	if err := registry.Register(aboutPrimitive(registry, hostVersion, &denied)); err != nil {
		return nil, err
	}
	for _, primitive := range primitives {
		if disabledSet[primitive.ID()] {
			denied = append(denied, DeniedEntry{
				Primitive: primitive.ID(), Reason: "disabled",
			})
			continue
		}
		if err := registry.Register(primitive); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// aboutPrimitive is host.about: the catalog this caller may call, and the
// primitives it may not, with the reason. It reads the registry it is
// registered into, so it reports what the connection will actually find
// rather than a list assembled separately from it.
func aboutPrimitive(registry *Registry, hostVersion string, denied *[]DeniedEntry) Primitive {
	return Primitive{
		Resource: "host", Op: "about",
		Description: "Report the host protocol and the primitives this caller may use",
		Handler: func(_ context.Context, call Call) (any, error) {
			out := About{
				Protocol:    ProtocolVersion,
				HostVersion: hostVersion,
				Denied:      append([]DeniedEntry(nil), *denied...),
			}
			// The catalog is what this caller can actually call: a
			// primitive whose grant the connection does not hold is
			// denied here rather than listed and then refused on the
			// next call. Denied carries only primitives that exist, so a
			// plugin never reads a grant it lacks as a primitive it has.
			for _, primitive := range registry.Primitives() {
				if primitive.Grant != "" && !call.Identity.Grants.Has(primitive.Grant) {
					out.Denied = append(out.Denied, DeniedEntry{
						Primitive: primitive.ID(), Reason: "no_grant",
					})
					continue
				}
				out.Primitives = append(out.Primitives, PrimitiveInfo{
					Primitive:   primitive.ID(),
					Grant:       primitive.Grant,
					Description: primitive.Description,
					Params:      primitive.Params,
				})
			}
			sort.Slice(out.Denied, func(i, j int) bool {
				return out.Denied[i].Primitive < out.Denied[j].Primitive
			})
			return out, nil
		},
	}
}
