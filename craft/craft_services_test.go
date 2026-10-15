package craft

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/craft/hostmcp"
)

// One stub per host service family: the binding resolves each family to
// its own interface, so telling the seven apart needs seven types.

type boundSecrets struct{}

func (boundSecrets) Get(context.Context, string, string) (string, error) { return "", nil }
func (boundSecrets) Set(context.Context, string, string, string) error   { return nil }
func (boundSecrets) Delete(context.Context, string, string) error        { return nil }

type boundContext struct{}

func (boundContext) Workspace(context.Context) (map[string]any, error) { return nil, nil }

type boundBrowser struct{}

func (boundBrowser) OpenURL(context.Context, string) error { return nil }

type boundInference struct{}

func (boundInference) Upsert(context.Context, string, map[string]any) (string, error) {
	return "", nil
}
func (boundInference) Remove(context.Context, string, string) error { return nil }

type boundSessions struct{}

func (boundSessions) Import(context.Context, string, map[string]any) (map[string]any, error) {
	return nil, nil
}
func (boundSessions) ImportedSources(context.Context, string) ([]string, error) { return nil, nil }

type boundTelemetry struct{}

func (boundTelemetry) Configure(context.Context, string, string, map[string]string) error {
	return nil
}
func (boundTelemetry) Disable(context.Context, string) error { return nil }

type boundEvents struct{}

func (boundEvents) Emit(context.Context, string, string, any) error { return nil }

// serviceBindingRows is the table the tests below drive: every family,
// the implementation registered under it, and a field reader for the
// services value it must end up on.
var serviceBindingRows = []struct {
	family string
	impl   any
	name   string
	field  func(hostmcp.Services) any
}{{
	family: "secrets", impl: boundSecrets{}, name: "vault",
	field: func(s hostmcp.Services) any { return s.Secrets },
}, {
	family: "workspace", impl: boundContext{}, name: "host",
	field: func(s hostmcp.Services) any { return s.Context },
}, {
	family: "browser", impl: boundBrowser{}, name: "desktop",
	field: func(s hostmcp.Services) any { return s.Browser },
}, {
	family: "inference", impl: boundInference{}, name: "gateway",
	field: func(s hostmcp.Services) any { return s.Inference },
}, {
	family: "sessions", impl: boundSessions{}, name: "importer",
	field: func(s hostmcp.Services) any { return s.Sessions },
}, {
	family: "telemetry", impl: boundTelemetry{}, name: "collector",
	field: func(s hostmcp.Services) any { return s.Telemetry },
}, {
	family: "events", impl: boundEvents{}, name: "feed",
	field: func(s hostmcp.Services) any { return s.Events },
}}

// TestApplyServiceBindingsResolvesEveryFamily pins what a binding does
// when it resolves: the named implementation lands on its own family's
// field, with the interface that family is bound to, and on no other.
func TestApplyServiceBindingsResolvesEveryFamily(t *testing.T) {
	t.Parallel()
	named := hostmcp.NewServiceRegistry()
	bindings := make(map[string]ServiceBinding, len(serviceBindingRows))
	for _, row := range serviceBindingRows {
		if err := named.Register(row.family, row.name, row.impl); err != nil {
			t.Fatalf("Register(%s.%s): %v", row.family, row.name, err)
		}
		bindings[row.family] = ServiceBinding{Impl: row.name}
	}
	var services hostmcp.Services
	if err := applyServiceBindings(bindings, named, &services); err != nil {
		t.Fatalf("applyServiceBindings: %v", err)
	}
	for _, row := range serviceBindingRows {
		got := row.field(services)
		if fmt.Sprintf("%T", got) != fmt.Sprintf("%T", row.impl) {
			t.Fatalf("services.%s = %T, want %T", row.family, got, row.impl)
		}
	}
}

// TestApplyServiceBindingsRefusals pins the three ways a binding is
// refused, each naming what is wrong with it.
func TestApplyServiceBindingsRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		family   string
		register string
		impl     any
		bind     string
		want     string
	}{{
		name: "unknown family",
		// A family craft does not know, and none registered either:
		// the definition is what is wrong, and the error has to say so
		// instead of sending the reader to whichever capability was
		// supposed to register the impl.
		family: "sandbox", bind: "box",
		want: `host_tools.services has unknown family "sandbox"`,
	}, {
		name:   "impl not registered",
		family: "secrets", register: "vault", impl: boundSecrets{}, bind: "ghost",
		want: `host_tools.services.secrets impl "ghost" is not registered`,
	}, {
		name:   "impl of the wrong type",
		family: "secrets", register: "vault", impl: boundEvents{}, bind: "vault",
		want: "want SecretService",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			named := hostmcp.NewServiceRegistry()
			if tc.register != "" {
				if err := named.Register(tc.family, tc.register, tc.impl); err != nil {
					t.Fatalf("Register: %v", err)
				}
			}
			var services hostmcp.Services
			err := applyServiceBindings(
				map[string]ServiceBinding{tc.family: {Impl: tc.bind}},
				named, &services)
			if !errdefs.IsValidation(err) {
				t.Fatalf("applyServiceBindings = %v, want a validation error", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("applyServiceBindings error = %v, want it to name %q",
					err, tc.want)
			}
			if services.Secrets != nil || services.Events != nil {
				t.Fatal("a refused binding left services set")
			}
		})
	}
}

// servicesCapability registers named implementations the way a
// capability does, so a definition can select one by name.
type servicesCapability struct{}

func (servicesCapability) Name() string { return "host-services" }

func (servicesCapability) Register(registry *resource.Registry) error {
	if err := event.Register(registry); err != nil {
		return err
	}
	return tool.Register(registry)
}

func (servicesCapability) RegisterServices(named *hostmcp.ServiceRegistry) error {
	if err := named.Register("secrets", "vault", boundSecrets{}); err != nil {
		return err
	}
	return named.Register("events", "feed", boundEvents{})
}

// TestCraftResolvesServiceBindingsFromTheDefinition pins the call site:
// a definition's host_tools.services reaches the binding resolution at
// all, so an implementation the capabilities never registered fails
// craft.New instead of leaving a host whose primitives quietly do
// nothing.
func TestCraftResolvesServiceBindingsFromTheDefinition(t *testing.T) {
	t.Parallel()
	definition := func(impl string) []byte {
		return []byte(`
craft:
  id: test
  version: 0.1.0
host_tools:
  services:
    secrets: {impl: ` + impl + `}
    events: {impl: feed}
deploy:
  version: v1
  resources:
    bus: {kind: event.Bus, impl: memory}
  runtime:
    event_bus: bus
`)
	}
	def, err := ParseDefinition(definition("vault"))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	c, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{servicesCapability{}},
	})
	if err != nil {
		t.Fatalf("New with registered impls: %v", err)
	}
	if c.hostMCP == nil {
		t.Fatal("bound services did not build a host MCP server")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	def, err = ParseDefinition(definition("ghost"))
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	if _, err := New(def, Options{
		DataDir:      t.TempDir(),
		Capabilities: []Capability{servicesCapability{}},
	}); !errdefs.IsValidation(err) {
		t.Fatalf("New with an unregistered impl = %v, want a validation error", err)
	}
}
