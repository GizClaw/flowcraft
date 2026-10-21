package server

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// notary is the application's half of the primitive set the tests below
// drive: a handler with a grant and one without, which is everything
// Catalog has to tell apart.
type notary struct {
	calls []string
}

func (n *notary) sign(_ context.Context, call Call) (any, error) {
	name, err := call.String("name")
	if err != nil {
		return nil, err
	}
	n.calls = append(n.calls, call.Identity.PluginID+":"+name)
	return map[string]any{"signed": name}, nil
}

// notaryPrimitives is one primitive that needs a grant and one that does
// not, so the catalog tests can tell the two apart.
func (n *notary) primitives() []Primitive {
	return []Primitive{
		{
			Resource: "notary", Op: "sign", Grant: "notary:sign",
			Description: "Sign one name",
			Params:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
			Handler:     n.sign,
		},
		{
			Resource: "notary", Op: "whoami",
			Description: "Report the calling plugin",
			Handler: func(_ context.Context, call Call) (any, error) {
				return map[string]any{"plugin": call.Identity.PluginID}, nil
			},
		},
	}
}

func aboutOf(t *testing.T, registry *Registry, identity Identity) About {
	t.Helper()
	primitive, ok := registry.Lookup("host.about")
	if !ok {
		t.Fatal("host.about is not registered")
	}
	out, err := primitive.Handler(context.Background(), Call{Identity: identity})
	if err != nil {
		t.Fatalf("host.about: %v", err)
	}
	about, ok := out.(About)
	if !ok {
		t.Fatalf("host.about returned %T, want About", out)
	}
	return about
}

// callableIDs is the catalog as a plain list of ids, which is how the tests
// that only care about the set read it.
func callableIDs(about About) []string {
	ids := make([]string, 0, len(about.Primitives))
	for _, entry := range about.Primitives {
		ids = append(ids, entry.Primitive)
	}
	return ids
}

func deniedReasons(about About) map[string]string {
	reasons := make(map[string]string, len(about.Denied))
	for _, entry := range about.Denied {
		reasons[entry.Primitive] = entry.Reason
	}
	return reasons
}

func TestNewServerRequiresARegistry(t *testing.T) {
	if _, err := NewServer(ServerOptions{}); !errdefs.IsValidation(err) {
		t.Fatalf("NewServer without a registry = %v, want a validation error", err)
	}
}

func TestNewServerDefaults(t *testing.T) {
	srv, err := NewServer(ServerOptions{Registry: NewRegistry()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()
	if srv.RuntimeDir() == "" {
		t.Fatal("the server has no runtime directory")
	}
	if srv.maxCallBytes != DefaultMaxCallBytes {
		t.Fatalf("max call bytes = %d, want %d", srv.maxCallBytes, DefaultMaxCallBytes)
	}
	if srv.limit != 20 {
		t.Fatalf("rate limit = %d, want 20", srv.limit)
	}
	if srv.Connections() != 0 {
		t.Fatalf("connections = %d, want 0", srv.Connections())
	}
}

func TestNewServerHonorsItsOptions(t *testing.T) {
	srv, err := NewServer(ServerOptions{
		Registry:     NewRegistry(),
		RuntimeDir:   t.TempDir(),
		RateLimit:    3,
		MaxCallBytes: 1024,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = srv.Close() }()
	if srv.maxCallBytes != 1024 || srv.limit != 3 {
		t.Fatalf("max call bytes = %d, rate limit = %d, want 1024 and 3",
			srv.maxCallBytes, srv.limit)
	}
}

func TestNewConnRequiresAPluginID(t *testing.T) {
	srv := newTestServer(t, ServerOptions{})
	if _, err := srv.NewConn(ConnOptions{}); !errdefs.IsValidation(err) {
		t.Fatalf("NewConn without an id = %v, want a validation error", err)
	}
}

func TestSocketPathBoundsItself(t *testing.T) {
	if _, err := socketPath(""); !errdefs.IsValidation(err) {
		t.Fatalf("socketPath(\"\") = %v, want a validation error", err)
	}
	long := strings.Repeat("a", maxSocketPath)
	if _, err := socketPath(long); !errdefs.IsValidation(err) {
		t.Fatalf("socketPath(long) = %v, want a validation error", err)
	}
	dir, err := os.MkdirTemp("", "wire")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path, err := socketPath(dir)
	if err != nil {
		t.Fatalf("socketPath: %v", err)
	}
	if len(path) > maxSocketPath {
		t.Fatalf("socket path %q is longer than %d", path, maxSocketPath)
	}
}

func TestRegistryRegister(t *testing.T) {
	registry := NewRegistry()
	handler := func(context.Context, Call) (any, error) { return nil, nil }
	if err := registry.Register(Primitive{Op: "probe", Handler: handler}); !errdefs.IsValidation(err) {
		t.Fatalf("Register without a resource = %v, want a validation error", err)
	}
	if err := registry.Register(Primitive{Resource: "probe", Op: "Call", Handler: handler}); !errdefs.IsValidation(err) {
		t.Fatalf("Register with a non-token op = %v, want a validation error", err)
	}
	if err := registry.Register(Primitive{Resource: "probe", Op: "call"}); !errdefs.IsValidation(err) {
		t.Fatalf("Register without a handler = %v, want a validation error", err)
	}
	probe := Primitive{Resource: "probe", Op: "call", Handler: handler}
	if err := registry.Register(probe); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register(probe); !errdefs.IsConflict(err) {
		t.Fatalf("duplicate Register = %v, want a conflict", err)
	}
	registered, ok := registry.Lookup("probe.call")
	if !ok {
		t.Fatal("Lookup did not find the registered primitive")
	}
	if !json.Valid(registered.Params) {
		t.Fatalf("default params schema %q is not JSON", registered.Params)
	}
	if err := registry.Register(Primitive{Resource: "another", Op: "call", Handler: handler}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, primitive := range registry.Primitives() {
		ids = append(ids, primitive.ID())
	}
	if want := []string{"another.call", "probe.call"}; !slices.Equal(ids, want) {
		t.Fatalf("Primitives = %v, want %v", ids, want)
	}
}

func TestCatalogServesTheApplicationsPrimitives(t *testing.T) {
	notary := &notary{}
	registry, err := Catalog(notary.primitives(), "test", nil)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	ids := make([]string, 0, 3)
	for _, primitive := range registry.Primitives() {
		ids = append(ids, primitive.ID())
	}
	want := []string{"host.about", "notary.sign", "notary.whoami"}
	if !slices.Equal(ids, want) {
		t.Fatalf("Catalog registered %v, want %v", ids, want)
	}
	// host.about answers for a connection holding the grant, and lists
	// the grant of each primitive it reports.
	about := aboutOf(t, registry, Identity{
		PluginID: "demo",
		Grants:   GrantSet{"notary:sign": true},
	})
	if about.Protocol != ProtocolVersion {
		t.Fatalf("protocol = %d, want %d", about.Protocol, ProtocolVersion)
	}
	if got := callableIDs(about); !slices.Equal(got, want) {
		t.Fatalf("catalog = %v, want %v", got, want)
	}
	if len(about.Denied) != 0 {
		t.Fatalf("denied = %v, want nothing", about.Denied)
	}
	for _, entry := range about.Primitives {
		if len(entry.Params) == 0 {
			t.Fatalf("%s carries no params schema", entry.Primitive)
		}
		if entry.Primitive == "notary.sign" && entry.Grant != "notary:sign" {
			t.Fatalf("notary.sign reports grant %q", entry.Grant)
		}
	}
}

func TestCatalogReportsMissingGrants(t *testing.T) {
	registry, err := Catalog((&notary{}).primitives(), "test", nil)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	about := aboutOf(t, registry, Identity{PluginID: "demo"})
	reasons := deniedReasons(about)
	if reasons["notary.sign"] != "no_grant" {
		t.Fatalf("notary.sign is denied for %q, want no_grant", reasons["notary.sign"])
	}
	// The catalog is what this caller can call: a primitive it is denied
	// is not listed as callable, and a primitive without a grant is
	// always callable.
	if got := callableIDs(about); !slices.Equal(got,
		[]string{"host.about", "notary.whoami"}) {
		t.Fatalf("catalog = %v, want host.about and the ungated primitive", got)
	}
}

func TestCatalogHidesDisabledPrimitives(t *testing.T) {
	registry, err := Catalog((&notary{}).primitives(), "test",
		[]string{"notary.whoami"})
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if _, ok := registry.Lookup("notary.whoami"); ok {
		t.Fatal("a disabled primitive is registered")
	}
	reasons := deniedReasons(aboutOf(t, registry, Identity{PluginID: "demo"}))
	if reasons["notary.whoami"] != "disabled" {
		t.Fatalf("notary.whoami is denied for %q, want disabled",
			reasons["notary.whoami"])
	}
}

func TestCatalogRefusesToHideHostAbout(t *testing.T) {
	// host.about is the primitive that reports what is exposed and what is
	// denied; hiding it would make the host's answer unavailable to the
	// very caller the answer is for.
	_, err := Catalog((&notary{}).primitives(), "test", []string{"host.about"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("disabling host.about = %v, want a validation error", err)
	}
}

func TestCatalogRefusesToDisableAnUnknownPrimitive(t *testing.T) {
	// An entry that names nothing reads as "off" in craft.yaml while every
	// plugin still sees the primitive, so a typo has to fail the build of
	// the host rather than be ignored.
	_, err := Catalog((&notary{}).primitives(), "test", []string{"notary.sig"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("disabling an unknown primitive = %v, want a validation error", err)
	}
}

func TestCatalogWithoutPrimitivesServesHostAbout(t *testing.T) {
	// A host with no application primitives is a valid host: host.about is
	// the whole surface, and it says so.
	registry, err := Catalog(nil, "test", nil)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	about := aboutOf(t, registry, Identity{PluginID: "demo"})
	if want := []string{"host.about"}; !slices.Equal(callableIDs(about), want) {
		t.Fatalf("catalog = %v, want %v", callableIDs(about), want)
	}
	if len(about.Denied) != 0 {
		t.Fatalf("denied = %v, want nothing", about.Denied)
	}
}

func TestRegistryGrants(t *testing.T) {
	registry, err := Catalog((&notary{}).primitives(), "test", nil)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if got := registry.Grants(); !slices.Equal(got, []string{"notary:sign"}) {
		t.Fatalf("Grants = %v, want the one grant the primitives need", got)
	}
}

func TestSplitPrimitiveID(t *testing.T) {
	resource, op, err := SplitPrimitiveID(" secret.get ")
	if err != nil {
		t.Fatalf("SplitPrimitiveID: %v", err)
	}
	if resource != "secret" || op != "get" {
		t.Fatalf("SplitPrimitiveID = %q, %q, want secret, get", resource, op)
	}
	for _, id := range []string{"", "secret", "secret.", ".get", "secret.get.x",
		"Secret.Get", "secret..get"} {
		if _, _, err := SplitPrimitiveID(id); !errdefs.IsValidation(err) {
			t.Fatalf("SplitPrimitiveID(%q) = %v, want a validation error", id, err)
		}
	}
}

func TestServerCloseIsIdempotent(t *testing.T) {
	srv := newTestServer(t, ServerOptions{})
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
