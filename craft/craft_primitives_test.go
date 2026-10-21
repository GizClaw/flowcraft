package craft

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	pluginserver "github.com/GizClaw/flowcraft/craft/plugin/server"
)

// primitiveCapability contributes host primitives and nothing else, which
// is what a capability that serves a plugin back looks like.
type primitiveCapability struct {
	name       string
	primitives []pluginserver.Primitive
}

func (c primitiveCapability) Name() string { return c.name }

func (primitiveCapability) Register(*resource.Registry) error { return nil }

func (c primitiveCapability) HostPrimitives() []pluginserver.Primitive {
	return c.primitives
}

// notaryPrimitives is one primitive behind a grant and one that is always
// allowed, so the tests can tell the catalog's two halves apart.
func notaryPrimitives() []pluginserver.Primitive {
	return []pluginserver.Primitive{{
		Resource: "notary", Op: "sign", Grant: "notary:sign",
		Description: "Sign one name",
		Params:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
		Handler: func(_ context.Context, call pluginserver.Call) (any, error) {
			name, err := call.String("name")
			if err != nil {
				return nil, err
			}
			return map[string]any{"signed": name}, nil
		},
	}, {
		Resource: "notary", Op: "whoami",
		Description: "Report the calling plugin",
		Handler: func(_ context.Context, call pluginserver.Call) (any, error) {
			return map[string]any{"plugin": call.Identity.PluginID}, nil
		},
	}}
}

// aboutOf asks the host craft built what it serves. It reads the wire
// server rather than a copy of the primitive list, so a primitive a
// capability declared and craft dropped is a finding, not a pass.
func aboutOf(
	t *testing.T,
	c *Craft,
	grants pluginserver.GrantSet,
) pluginserver.About {
	t.Helper()
	if c.hostWire == nil {
		t.Fatal("craft built no wire server")
	}
	primitive, ok := c.hostWire.server.Registry().Lookup("host.about")
	if !ok {
		t.Fatal("the wire server does not serve host.about")
	}
	out, err := primitive.Handler(context.Background(), pluginserver.Call{
		Identity: pluginserver.Identity{PluginID: "demo", Grants: grants},
	})
	if err != nil {
		t.Fatalf("host.about: %v", err)
	}
	about, ok := out.(pluginserver.About)
	if !ok {
		t.Fatalf("host.about returned %T, want pluginserver.About", out)
	}
	return about
}

// callableIDs is the catalog as a plain list of ids.
func callableIDs(about pluginserver.About) []string {
	ids := make([]string, 0, len(about.Primitives))
	for _, entry := range about.Primitives {
		ids = append(ids, entry.Primitive)
	}
	return ids
}

// deniedReasons maps a denied primitive to the reason it was denied.
func deniedReasons(about pluginserver.About) map[string]string {
	reasons := make(map[string]string, len(about.Denied))
	for _, entry := range about.Denied {
		reasons[entry.Primitive] = entry.Reason
	}
	return reasons
}

// newPrimitiveCraft assembles a Craft over the plugin test definition with
// the given capabilities contributing primitives. The definition is the one
// the node engine capability can deploy, so the extra capabilities have to
// register no resources to use it.
func newPrimitiveCraft(
	t *testing.T,
	hostTools string,
	caps ...Capability,
) (*Craft, error) {
	t.Helper()
	def, err := ParseDefinition([]byte(
		pluginTestDefinition + hostTools))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	return New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: caps,
		Plugins:      newNodeHost(),
	})
}

// TestCraftServesThePrimitivesItsCapabilitiesContribute is the wiring from
// a capability to a plugin: what the application declares is what the wire
// serves, and host.about reports each primitive's grant.
func TestCraftServesThePrimitivesItsCapabilitiesContribute(t *testing.T) {
	t.Parallel()
	c, err := newPrimitiveCraft(t, "", primitiveCapability{
		name: "notary", primitives: notaryPrimitives(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	about := aboutOf(t, c, pluginserver.GrantSet{"notary:sign": true})
	want := []string{"host.about", "notary.sign", "notary.whoami"}
	if got := callableIDs(about); !slices.Equal(got, want) {
		t.Fatalf("catalog = %v, want %v", got, want)
	}
	if len(about.Denied) != 0 {
		t.Fatalf("denied = %v, want nothing", about.Denied)
	}
	if about.HostVersion != Version {
		t.Fatalf("host version = %q, want %q", about.HostVersion, Version)
	}
	for _, entry := range about.Primitives {
		if len(entry.Params) == 0 {
			t.Fatalf("%s carries no params schema", entry.Primitive)
		}
	}
	if got := c.hostWire.server.Registry().Grants(); !slices.Equal(
		got, []string{"notary:sign"}) {
		t.Fatalf("grants = %v, want the primitive's grant", got)
	}
}

// TestCraftWithoutPrimitivesStillServesHostAbout pins the empty catalog: a
// craft with a plugin host and no primitives still gives a plugin somewhere
// to ask, because host.about is the surface then.
func TestCraftWithoutPrimitivesStillServesHostAbout(t *testing.T) {
	t.Parallel()
	c, err := newPrimitiveCraft(t, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	about := aboutOf(t, c, nil)
	if want := []string{"host.about"}; !slices.Equal(callableIDs(about), want) {
		t.Fatalf("catalog = %v, want %v", callableIDs(about), want)
	}
	if len(about.Denied) != 0 {
		t.Fatalf("denied = %v, want nothing", about.Denied)
	}
}

// TestCraftRefusesDuplicatePrimitives covers two capabilities claiming one
// id: one of them would silently win, so the build of the host fails.
func TestCraftRefusesDuplicatePrimitives(t *testing.T) {
	t.Parallel()
	_, err := newPrimitiveCraft(t, "",
		primitiveCapability{name: "first", primitives: notaryPrimitives()},
		primitiveCapability{name: "second", primitives: notaryPrimitives()},
	)
	if !errdefs.IsConflict(err) || !strings.Contains(err.Error(), "notary.sign") {
		t.Fatalf("New with a duplicate primitive = %v, want a Conflict naming it", err)
	}
}

// TestCraftHidesDisabledPrimitives covers host_tools.disable: the primitive
// is not served, and host.about says why, so a plugin reads the same answer
// the configuration chose.
func TestCraftHidesDisabledPrimitives(t *testing.T) {
	t.Parallel()
	c, err := newPrimitiveCraft(t, `
host_tools:
  disable: [notary.whoami]
`, primitiveCapability{name: "notary", primitives: notaryPrimitives()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	about := aboutOf(t, c, pluginserver.GrantSet{"notary:sign": true})
	if got, want := callableIDs(about),
		[]string{"host.about", "notary.sign"}; !slices.Equal(got, want) {
		t.Fatalf("catalog = %v, want %v", got, want)
	}
	reasons := deniedReasons(about)
	if reasons["notary.whoami"] != "disabled" {
		t.Fatalf("notary.whoami is denied for %q, want disabled",
			reasons["notary.whoami"])
	}
}

// TestCraftRefusesToDisableAPrimitiveNobodyContributes pins the other half
// of the same configuration: an entry naming nothing would read as "off"
// while every plugin still sees the primitive.
func TestCraftRefusesToDisableAPrimitiveNobodyContributes(t *testing.T) {
	t.Parallel()
	_, err := newPrimitiveCraft(t, `
host_tools:
  disable: [notary.sig]
`, primitiveCapability{name: "notary", primitives: notaryPrimitives()})
	if !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "notary.sig") {
		t.Fatalf("New with an unknown disable entry = %v, want a Validation error",
			err)
	}
}
