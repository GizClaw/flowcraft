// Command server is the hello plugin: a plugin process speaking craft's
// plugin wire.
//
// craft starts it and hands it the socket to dial in
// FLOWCRAFT_PLUGIN_WIRE; everything the plugin contributes — its two tools
// — is declared over that connection, and every primitive it calls travels
// back on it. The process is therefore a call to protocol.Main and a
// handler: there is no port to open, no token to read, and no transport to
// choose.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/plugin/protocol"
)

func main() {
	handler := &hello{}
	if err := protocol.Main(context.Background(), handler, protocol.ClientOptions{
		PluginID:    os.Getenv(protocol.IDEnv),
		Version:     "0.1.0",
		Concurrency: 2,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "hello:", err)
		os.Exit(1)
	}
}

// hello is the plugin's whole surface. The host client arrives with
// Declare because a connection is what a plugin is granted primitives on,
// and Call is answered on that same connection.
type hello struct {
	mu   sync.RWMutex
	host protocol.HostClient
}

// Declare publishes the plugin's tools. The schemas are JSON, which is what
// lets craft mount them without knowing what language the plugin is written
// in, and what makes them ordinary tools to every runtime.
func (h *hello) Declare(
	_ context.Context,
	_ *protocol.HelloResponse,
	host protocol.HostClient,
) (*protocol.Declaration, error) {
	h.mu.Lock()
	h.host = host
	h.mu.Unlock()
	return &protocol.Declaration{Tools: []*protocol.ToolDecl{
		{
			Name:        "greet",
			Description: "Greet one name (the hello plugin's own tool).",
			InputSchema: []byte(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "who to greet"}
				},
				"required": ["name"],
				"additionalProperties": false
			}`),
		},
		{
			Name: "ping_host",
			Description: "Call the host primitives host.about and events.emit " +
				"and report the result.",
			InputSchema: []byte(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
		},
	}}, nil
}

// Call answers one invocation. The result is bytes: craft hands a JSON
// result to the caller as it is, and a result that is not JSON — the plain
// text this plugin answers with — as text.
func (h *hello) Call(
	ctx context.Context,
	request *protocol.CallRequest,
) ([]byte, error) {
	switch request.GetTool() {
	case "greet":
		return greet(request.GetArgumentsJson())
	case "ping_host":
		return h.pingHost(ctx)
	default:
		return nil, errdefs.NotFoundf(
			"hello: no tool named %q", request.GetTool())
	}
}

// greetTool is the plugin's own tool: pure, no host access.
func greet(arguments []byte) ([]byte, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return nil, errdefs.Validationf("hello: decode arguments: %w", err)
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return nil, errdefs.Validationf("hello: name is required")
	}
	return []byte("Hello, " + name + "! (from the hello plugin)"), nil
}

// pingHost exercises the host primitive contract from the plugin side: one
// call that reports the host, one that publishes an event. The plugin's
// manifest grants events:emit and not secrets:auth, which is why events.emit
// works here and the secret.* primitives are reported as needing a grant.
func (h *hello) pingHost(ctx context.Context) ([]byte, error) {
	h.mu.RLock()
	host := h.host
	h.mu.RUnlock()
	if host == nil {
		return nil, errdefs.NotAvailablef("hello: no host connection")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var about struct {
		Protocol    int    `json:"protocol"`
		HostVersion string `json:"host_version"`
		Primitives  []struct {
			Primitive string `json:"primitive"`
			Grant     string `json:"grant"`
		} `json:"primitives"`
		Denied []struct {
			Primitive string `json:"primitive"`
			Reason    string `json:"reason"`
		} `json:"denied"`
	}
	err := protocol.Invoke(ctx, host, "host", "about", nil, &about)
	if err != nil {
		return nil, fmt.Errorf("hello: host.about: %w", err)
	}
	if err := protocol.Invoke(ctx, host, "events", "emit", map[string]any{
		"subject": "ping",
		"payload": map[string]any{"from": "hello"},
	}, nil); err != nil {
		return nil, fmt.Errorf("hello: events.emit: %w", err)
	}

	// host.about reports the catalog this connection may call, plus the
	// primitives it may not: no_grant means the plugin's manifest does not
	// ask for the permission the primitive needs, disabled means the
	// application switched the primitive off on purpose.
	var grants, disabled []string
	for _, entry := range about.Denied {
		switch entry.Reason {
		case "no_grant":
			grants = append(grants, entry.Primitive)
		case "disabled":
			disabled = append(disabled, entry.Primitive)
		}
	}
	callable := make([]string, 0, len(about.Primitives))
	for _, entry := range about.Primitives {
		callable = append(callable, entry.Primitive)
	}
	summary := fmt.Sprintf(
		"host protocol %d (host version %s): %d primitives callable by this plugin [%s]",
		about.Protocol, about.HostVersion, len(callable),
		strings.Join(callable, ", "))
	if len(grants) > 0 {
		summary += fmt.Sprintf("; needs a grant for [%s]", strings.Join(grants, ", "))
	}
	if len(disabled) > 0 {
		summary += fmt.Sprintf("; disabled by the host [%s]", strings.Join(disabled, ", "))
	}
	return []byte(summary), nil
}
