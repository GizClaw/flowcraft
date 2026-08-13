package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ResourceKind is the deployment resource kind implemented by this
// package.
const ResourceKind = "tool.Source"

// Spec is the declarative shape of an MCP source declaration.
type Spec struct {
	Servers []ServerSpec `json:"servers"`
}

// ServerSpec declares one server attachment.
type ServerSpec struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Prefix    *string           `json:"prefix,omitempty"`
	Resources bool              `json:"resources,omitempty"`
}

// Transport constants for ServerSpec.Transport.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// Factory builds a tool.Source resource that connects every declared MCP
// server and exposes its tools.
type Factory struct{}

// NewFactory returns an MCP tool source factory.
func NewFactory() Factory { return Factory{} }

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "mcp"}
}

// New implements resource.Factory.
func (Factory) New(ctx context.Context, in resource.Input) (any, error) {
	parsed, err := ParseSpec(in.Settings)
	if err != nil {
		return nil, err
	}
	source := NewSource()
	for _, srv := range parsed.Servers {
		transport, err := srv.transport()
		if err != nil {
			_ = source.Close()
			return nil, err
		}
		if err := source.AddServer(ctx, srv.Name, transport, srv.options()...); err != nil {
			_ = source.Close()
			return nil, fmt.Errorf("mcp: attach server %q: %w", srv.Name, err)
		}
	}
	return source, nil
}

// Register adds the MCP tool source factory to r.
func Register(r *resource.Registry) error {
	return r.Register(Factory{})
}

// ParseSpec strictly decodes an MCP source spec.
func ParseSpec(settings json.RawMessage) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](settings, resource.ExpandEnv())
	if err != nil {
		return Spec{}, fmt.Errorf("mcp: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// Validate checks the spec's own invariants.
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
	if s.Prefix != nil {
		opts = append(opts, WithPrefix(*s.Prefix))
	}
	if s.Resources {
		opts = append(opts, WithResources(true))
	}
	return opts
}
