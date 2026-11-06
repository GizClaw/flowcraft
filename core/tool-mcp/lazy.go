package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	sdktool "github.com/GizClaw/flowcraft/core/tool"
)

// LazyPrefix names the server-level placeholder a lazy source publishes.
//
// One descriptor per server, not per tool: a server's tools are only known after
// it is connected, so a per-tool descriptor would require the very connection
// deferral is meant to avoid.
const LazyPrefix = "mcp:"

// LazyToolName is the placeholder name for one server.
func LazyToolName(server string) string { return LazyPrefix + server }

// NewLazy builds a source that starts nothing.
//
// Each server publishes one descriptor whose load connects it and publishes its
// tools through the attached registrar. That is the difference that matters in
// practice: Connect starts every configured server, so a deployment with ten
// servers pays ten processes whether or not the model ever uses them.
//
// A server that cannot start therefore fails when it is loaded, not when the
// runtime is assembled — the error surfaces where the model asked for it.
func NewLazy(spec Spec, opts ...Option) (*Source, error) {
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
	source.lazy = make([]sdktool.LazyTool, 0, len(spec.Servers))
	for _, server := range spec.Servers {
		placeholder, err := placeholderDefinition(server)
		if err != nil {
			return nil, err
		}
		source.lazy = append(source.lazy, sdktool.LazyTool{
			Name:        LazyToolName(server.Name),
			Placeholder: placeholder,
			Load:        source.loader(server, settings.httpClient),
		})
	}
	return source, nil
}

// SetConnectTimeout bounds each connection attempt made by a lazy load. A
// non-positive value keeps the current bound.
func (s *Source) SetConnectTimeout(d time.Duration) {
	if d > 0 {
		s.mu.Lock()
		s.connectTimeout = d
		s.mu.Unlock()
	}
}

// placeholderDefinition is what the model sees before the server starts: the
// name it can search for and the description a person wrote.
func placeholderDefinition(server ServerSpec) (message.ToolDefinition, error) {
	schema, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	})
	if err != nil {
		return message.ToolDefinition{}, err
	}
	description := strings.TrimSpace(server.Description)
	if description == "" {
		description = fmt.Sprintf("Tools served by the %q MCP server.", server.Name)
	}
	return message.ToolDefinition{
		Name: LazyToolName(server.Name),
		Description: description + " Not connected yet: loading this entry starts the " +
			"server and publishes its tools for the next round.",
		InputSchema: schema,
	}, nil
}

// loader starts one server on first use.
func (s *Source) loader(server ServerSpec, client *http.Client) func(context.Context) (sdktool.Tool, error) {
	return func(ctx context.Context) (sdktool.Tool, error) {
		transport, err := server.transport(client)
		if err != nil {
			return nil, err
		}
		options, err := server.options()
		if err != nil {
			return nil, err
		}
		if err := s.AddServer(ctx, server.Name, transport, options...); err != nil {
			return nil, fmt.Errorf("mcp: start server %q: %w", server.Name, err)
		}
		// Wait rather than returning immediately: a caller loads a server so the
		// next round can see its tools, and a background connection would make
		// that promise false.
		if err := s.WaitReady(ctx, server.Name, s.connectTimeout); err != nil {
			return nil, err
		}
		s.forgetLazy(server.Name)
		return connectedTool{server: server.Name}, nil
	}
}

// forgetLazy drops the descriptor once its server is up, so a later LazyTools
// call does not offer to load it twice.
func (s *Source) forgetLazy(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.lazy[:0]
	for _, descriptor := range s.lazy {
		if descriptor.Name != LazyToolName(name) {
			kept = append(kept, descriptor)
		}
	}
	s.lazy = kept
}

// connectedTool is what the placeholder becomes: an acknowledgement that names
// the server and reports that its tools are now published.
type connectedTool struct{ server string }

var _ sdktool.Tool = connectedTool{}

func (c connectedTool) Definition() message.ToolDefinition {
	schema, _ := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	})
	return message.ToolDefinition{
		Name: LazyToolName(c.server),
		Description: fmt.Sprintf(
			"The %q MCP server is connected; its tools are published for the next round.",
			c.server),
		InputSchema: schema,
	}
}

func (c connectedTool) Execute(context.Context, string) (message.Content, error) {
	return message.NewTextContent(fmt.Sprintf(
		"MCP server %q is connected. Its tools are now listed; call them by their published names.",
		c.server)), nil
}
