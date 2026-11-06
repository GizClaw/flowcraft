package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/httpkit"
	"github.com/GizClaw/flowcraft/core/telemetry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Spec is the deployment declaration of the MCP servers one source attaches to.
//
// It is plain JSON decoded here rather than by the deployment layer: the shape is
// this package's own, and an application hands the sub-document over as-is.
type Spec struct {
	Servers []ServerSpec `json:"servers"`
}

// ServerSpec declares one server attachment. Which fields apply depends on the
// transport, and the validation below rejects the ones that do not rather than
// ignoring them.
type ServerSpec struct {
	Name        string            `json:"name"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	HTTPTimeout *string           `json:"http_timeout,omitempty"`
	Liveness    *string           `json:"liveness,omitempty"`
	Prefix      *string           `json:"prefix,omitempty"`
	// Description tells a model what the server offers before it is connected.
	// It is what makes a lazy server findable: without it, the placeholder says
	// only a name, and search has nothing to rank.
	Description string `json:"description,omitempty"`
	// Resources and Required are pointers because "absent" is not "false": an
	// absent value keeps the package default, while an explicit false overrides it.
	Resources *bool `json:"resources,omitempty"`
	Required  *bool `json:"required,omitempty"`
}

// Transport constants for ServerSpec.Transport.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// Decode strictly decodes the settings an application passes to [Connect].
//
// Unknown fields are errors: a misspelled key must not silently become a default,
// which is the failure a permissive decoder hides until a server fails to attach.
func Decode(raw json.RawMessage) (Spec, error) {
	spec := Spec{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, errdefs.Validationf("mcp: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// Option configures [Connect].
type Option func(*connectOptions)

type connectOptions struct {
	httpClient *http.Client
}

// WithHTTPClient injects the HTTP client used for streamable-HTTP servers. A nil
// client is ignored, leaving the hardened default.
func WithHTTPClient(client *http.Client) Option {
	return func(o *connectOptions) {
		if client != nil {
			o.httpClient = client
		}
	}
}

// Connect attaches every server the spec declares and returns a source exposing
// their tools. A partially built source is closed before the error is returned, so
// a failed connection leaks no process or connection.
func Connect(ctx context.Context, spec Spec, opts ...Option) (*Source, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	settings := connectOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}

	source := NewSource()
	for _, server := range spec.Servers {
		transport, err := server.transport(settings.httpClient)
		if err != nil {
			closeQuietly(ctx, source, "transport failure")
			return nil, err
		}
		serverOpts, err := server.options()
		if err != nil {
			closeQuietly(ctx, source, "option failure")
			return nil, err
		}
		if err := source.AddServer(ctx, server.Name, transport, serverOpts...); err != nil {
			closeQuietly(ctx, source, "attach failure")
			return nil, fmt.Errorf("mcp: attach server %q: %w", server.Name, err)
		}
	}
	return source, nil
}

// closeQuietly releases a partially built source, reporting the release failure
// without masking the original one.
func closeQuietly(ctx context.Context, source *Source, stage string) {
	if err := source.Close(); err != nil {
		telemetry.WarnErr(ctx, "mcp: close partial source after "+stage, err)
	}
}

// Validate checks the spec's own invariants.
func (s Spec) Validate() error {
	if len(s.Servers) == 0 {
		return errdefs.Validationf("mcp: spec must declare at least one server")
	}
	seen := make(map[string]struct{}, len(s.Servers))
	for i, server := range s.Servers {
		if strings.TrimSpace(server.Name) == "" {
			return errdefs.Validationf("mcp: servers[%d]: name is required", i)
		}
		if _, duplicate := seen[server.Name]; duplicate {
			return errdefs.Validationf("mcp: servers[%d]: duplicate name %q", i, server.Name)
		}
		seen[server.Name] = struct{}{}
		if err := server.validate(i); err != nil {
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
	if s.Liveness != nil {
		if _, err := parseLiveness(*s.Liveness); err != nil {
			return errdefs.Validationf(
				"mcp: servers[%d] (%s): liveness must be \"off\" or a positive duration",
				index, s.Name)
		}
	}
	return nil
}

// parseLiveness parses a per-server liveness setting: "off" disables probing,
// returning zero, and any other value must be a positive duration.
func parseLiveness(raw string) (time.Duration, error) {
	if strings.EqualFold(strings.TrimSpace(raw), "off") {
		return 0, nil
	}
	interval, err := time.ParseDuration(raw)
	if err != nil || interval <= 0 {
		return 0, fmt.Errorf(`want "off" or a positive duration`)
	}
	return interval, nil
}

// transport builds the transport this spec describes.
func (s ServerSpec) transport(client *http.Client) (mcpsdk.Transport, error) {
	switch s.Transport {
	case TransportStdio:
		return Stdio(s.Command, s.Args, s.Env)
	case TransportHTTP:
		if s.HTTPTimeout != nil && client == nil {
			timeout, err := time.ParseDuration(*s.HTTPTimeout)
			if err != nil {
				return nil, errdefs.Validationf(
					"mcp: server %q: http_timeout: %w", s.Name, err)
			}
			client = httpkit.NewHTTPClient(httpkit.WithTimeout(timeout))
		}
		return StreamableHTTP(s.URL, s.Headers, client)
	default:
		return nil, errdefs.Validationf(
			"mcp: server %q: unknown transport %q", s.Name, s.Transport)
	}
}

// options translates the declaration into ServerOptions.
func (s ServerSpec) options() ([]ServerOption, error) {
	var opts []ServerOption
	if s.Prefix != nil {
		opts = append(opts, WithPrefix(*s.Prefix))
	}
	if s.Resources != nil {
		opts = append(opts, WithResources(*s.Resources))
	}
	if s.Required != nil && *s.Required {
		opts = append(opts, WithRequired())
	}
	if s.Liveness != nil {
		interval, err := parseLiveness(*s.Liveness)
		if err != nil {
			return nil, errdefs.Validationf("mcp: server %q: liveness: %w", s.Name, err)
		}
		opts = append(opts, WithServerLiveness(interval))
	}
	return opts, nil
}
