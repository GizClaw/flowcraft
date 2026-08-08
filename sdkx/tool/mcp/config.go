package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SpecKind is the conventional document kind for an MCP source entry.
const SpecKind = "mcp"

// Spec is the declarative shape of an MCP source declaration:
//
//	sources:
//	  - kind: mcp
//	    spec:
//	      servers:
//	        - name: filesystem
//	          transport: stdio
//	          command: npx
//	          args: ["-y", "@modelcontextprotocol/server-filesystem", "/srv/data"]
//	        - name: github
//	          transport: http
//	          url: https://mcp.example.com/v1
//	          headers:
//	            Authorization: "Bearer ${GITHUB_TOKEN}"
//	          scope: platform
//
// Interpolation of ${...} is not this package's concern: the host
// resolves secrets before handing the document over, so credentials
// never have to live in a checked-in file.
type Spec struct {
	Servers []ServerSpec `json:"servers"`
}

// ServerSpec declares one server attachment.
type ServerSpec struct {
	// Name identifies the server and, by default, prefixes its tool
	// names as "<name>__".
	Name string `json:"name"`
	// Transport selects the binding: "stdio" or "http".
	Transport string `json:"transport"`

	// Stdio fields.
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// HTTP fields.
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Scope is the registry scope the server's tools register under:
	// "agent" (default, visible to models) or "platform" (callable by
	// explicit name, hidden from listings).
	Scope string `json:"scope,omitempty"`
	// Prefix overrides the default "<name>__" namespace. An explicit
	// empty string registers tools under their bare server-side names.
	Prefix *string `json:"prefix,omitempty"`
}

// Transport constants for ServerSpec.Transport.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// configSource adapts a Source to the config layer's Source interface.
// The registry arrives at Attach time rather than construction because
// the factory runs before the Builder hands its registry over.
type configSource struct {
	spec   Spec
	source *Source
}

var _ toolconfig.Source = (*configSource)(nil)

// SourceFactory decodes an MCP source spec into an unattached source,
// for registration on a config.Builder:
//
//	builder.RegisterSourceFactory(mcp.SpecKind, mcp.SourceFactory)
//
// The dependency direction matches sdkx/inference: the integration
// package imports the config package, never the reverse, so a host that
// never mentions MCP does not link the MCP SDK.
func SourceFactory(_ context.Context, in sdkconfig.Input) (toolconfig.Source, error) {
	parsed, err := ParseSpec(in.Settings)
	if err != nil {
		return nil, err
	}
	return &configSource{spec: parsed}, nil
}

// ParseSpec strictly decodes an MCP source spec, rejecting unknown
// fields so a typo in a server declaration fails at build time instead
// of silently omitting a server.
func ParseSpec(settings json.RawMessage) (Spec, error) {
	spec, err := toolconfig.DecodeSpec[Spec](settings)
	if err != nil {
		return Spec{}, fmt.Errorf("mcp: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// Validate checks the spec's own invariants: at least one server, no
// duplicate names, a known transport, and the fields that transport
// requires.
func (s Spec) Validate() error {
	if len(s.Servers) == 0 {
		return errdefs.Validationf("mcp: spec must declare at least one server")
	}
	seen := make(map[string]struct{}, len(s.Servers))
	for i, srv := range s.Servers {
		if strings.TrimSpace(srv.Name) == "" {
			return errdefs.Validationf("mcp: servers[%d]: name is required", i)
		}
		if _, dup := seen[srv.Name]; dup {
			return errdefs.Validationf("mcp: servers[%d]: duplicate name %q", i, srv.Name)
		}
		seen[srv.Name] = struct{}{}
		if err := srv.validate(i); err != nil {
			return err
		}
	}
	return nil
}

func (s ServerSpec) validate(index int) error {
	switch s.Scope {
	case "", sdktool.ScopeAgent, sdktool.ScopePlatform:
	default:
		return errdefs.Validationf(
			"mcp: servers[%d] (%s): scope %q is not %q or %q",
			index, s.Name, s.Scope, sdktool.ScopeAgent, sdktool.ScopePlatform)
	}
	switch s.Transport {
	case TransportStdio:
		if s.Command == "" {
			return errdefs.Validationf(
				"mcp: servers[%d] (%s): stdio transport requires a command",
				index, s.Name)
		}
		if s.URL != "" || len(s.Headers) > 0 {
			return errdefs.Validationf(
				"mcp: servers[%d] (%s): url/headers are http fields, not stdio",
				index, s.Name)
		}
	case TransportHTTP:
		if s.URL == "" {
			return errdefs.Validationf(
				"mcp: servers[%d] (%s): http transport requires a url",
				index, s.Name)
		}
		if s.Command != "" || len(s.Args) > 0 || len(s.Env) > 0 {
			return errdefs.Validationf(
				"mcp: servers[%d] (%s): command/args/env are stdio fields, not http",
				index, s.Name)
		}
	case "":
		return errdefs.Validationf(
			"mcp: servers[%d] (%s): transport is required (%q or %q)",
			index, s.Name, TransportStdio, TransportHTTP)
	default:
		return errdefs.Validationf(
			"mcp: servers[%d] (%s): unknown transport %q (want %q or %q)",
			index, s.Name, s.Transport, TransportStdio, TransportHTTP)
	}
	return nil
}

// transport builds the go-sdk transport this spec describes.
func (s ServerSpec) transport() (mcpsdk.Transport, error) {
	switch s.Transport {
	case TransportStdio:
		return Stdio(s.Command, s.Args, s.Env)
	case TransportHTTP:
		return StreamableHTTP(s.URL, s.Headers, http.DefaultClient)
	default:
		return nil, errdefs.Validationf(
			"mcp: server %q: unknown transport %q", s.Name, s.Transport)
	}
}

// options translates the declarative spec into ServerOptions.
func (s ServerSpec) options() []ServerOption {
	var opts []ServerOption
	if s.Scope != "" {
		opts = append(opts, WithScope(s.Scope))
	}
	if s.Prefix != nil {
		opts = append(opts, WithPrefix(*s.Prefix))
	}
	return opts
}

// Attach builds a Source over registry and connects every declared
// server. A failure on any server closes the ones already attached, so
// a bad entry never leaves child processes running.
func (c *configSource) Attach(ctx context.Context, registry *sdktool.Registry) error {
	if registry == nil {
		return errdefs.Validationf("mcp: attach: registry is nil")
	}
	source := NewSource(registry)
	for _, srv := range c.spec.Servers {
		transport, err := srv.transport()
		if err != nil {
			_ = source.Close()
			return err
		}
		if err := source.AddServer(ctx, srv.Name, transport, srv.options()...); err != nil {
			_ = source.Close()
			return fmt.Errorf("mcp: attach server %q: %w", srv.Name, err)
		}
	}
	c.source = source
	return nil
}

// Close releases the underlying Source. Idempotent.
func (c *configSource) Close() error {
	if c.source == nil {
		return nil
	}
	err := c.source.Close()
	c.source = nil
	return err
}

// Source exposes the live Source once attached, for hosts that need to
// call Refresh or inspect attached servers. Returns nil before Attach.
func (c *configSource) Source() *Source { return c.source }
