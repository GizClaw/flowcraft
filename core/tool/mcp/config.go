package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/utils"
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
	Name        string            `json:"name"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	HTTPTimeout *string           `json:"http_timeout,omitempty"`
	Prefix      *string           `json:"prefix,omitempty"`
	Resources   resource.Bool     `json:"resources,omitempty"`
	Required    resource.Bool     `json:"required,omitempty"`
}

// Transport constants for ServerSpec.Transport.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// Factory builds a tool.Source resource that connects every declared MCP
// server and exposes its tools.
type Factory struct {
	httpClient *http.Client
}

// FactoryOption configures an MCP tool source factory.
type FactoryOption func(*Factory)

// WithHTTPClient injects the HTTP client used for streamable-HTTP MCP
// servers. A nil client is ignored and the factory falls back to the
// hardened core/utils client.
func WithHTTPClient(client *http.Client) FactoryOption {
	return func(f *Factory) {
		if client != nil {
			f.httpClient = client
		}
	}
}

// NewFactory returns an MCP tool source factory.
func NewFactory(opts ...FactoryOption) Factory {
	f := Factory{}
	for _, opt := range opts {
		if opt != nil {
			opt(&f)
		}
	}
	return f
}

// Spec implements resource.Factory.
func (Factory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: "mcp"}
}

// New implements resource.Factory.
func (f Factory) New(ctx context.Context, in resource.Input) (any, error) {
	parsed, err := ParseSpec(in.Settings)
	if err != nil {
		return nil, err
	}
	source := NewSource()
	for _, srv := range parsed.Servers {
		transport, err := srv.transport(f.httpClient)
		if err != nil {
			if cerr := source.Close(); cerr != nil {
				telemetry.WarnErr(ctx, "mcp: close partial source after transport failure", cerr)
			}
			return nil, err
		}
		if err := source.AddServer(ctx, srv.Name, transport, srv.options()...); err != nil {
			if cerr := source.Close(); cerr != nil {
				telemetry.WarnErr(ctx, "mcp: close partial source after attach failure", cerr)
			}
			return nil, fmt.Errorf("mcp: attach server %q: %w", srv.Name, err)
		}
	}
	return source, nil
}

// Register adds the MCP tool source factory to r.
func Register(r *resource.Registry) error {
	return r.Register(NewFactory())
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
		if s.HTTPTimeout != nil {
			timeout, err := time.ParseDuration(*s.HTTPTimeout)
			if err != nil || timeout <= 0 {
				return errdefs.Validationf(
					"mcp: servers[%d] (%s): http_timeout must be a positive duration",
					index, s.Name)
			}
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
func (s ServerSpec) transport(client *http.Client) (mcpsdk.Transport, error) {
	switch s.Transport {
	case TransportStdio:
		return Stdio(s.Command, s.Args, s.Env)
	case TransportHTTP:
		if s.HTTPTimeout != nil && client == nil {
			timeout, err := time.ParseDuration(*s.HTTPTimeout)
			if err != nil {
				return nil, errdefs.Validationf(
					"mcp: server %q: http_timeout: %v", s.Name, err)
			}
			client = utils.NewHttpClient(utils.WithTimeout(timeout))
		}
		return StreamableHTTP(s.URL, s.Headers, client)
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
	if s.Required {
		opts = append(opts, WithRequired())
	}
	return opts
}
