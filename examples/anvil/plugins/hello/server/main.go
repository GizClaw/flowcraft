// Command server is the hello plugin's MCP tool server.
//
// A plugin's `mcp` section names one MCP server. craft's plugin host
// starts it, aggregates its tools into the shared tool set that every
// runtime's tool assembly mounts, and prefixes their names with the
// plugin id — so `greet` reaches the model as `hello__greet`.
//
// This is an ordinary stdio MCP server on the official go-sdk. The
// manifest starts it with `go run ./server`, which keeps the plugin
// source-only and readable; a real plugin ships a compiled binary (or an
// interpreter plus a script) instead of compiling at launch.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "hello",
		Version: "0.1.0",
	}, nil)
	server.AddTool(greetTool(), greet)
	server.AddTool(pingHostTool(), pingHost)
	if err := server.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil &&
		!isPeerShutdown(err) {
		fmt.Fprintln(os.Stderr, "hello:", err)
		os.Exit(1)
	}
}

// isPeerShutdown reports the shutdown the MCP stdio contract prescribes:
// the host closes the child's stdin and waits for it to exit. The SDK
// surfaces that as "server is closing: EOF" (a wrapped -32004 around a
// pipe EOF) without exporting a sentinel to match on, so the message is
// what is left. Exiting non-zero here would be reported by the plugin
// host as a failed stop, which is what this keeps clean.
func isPeerShutdown(err error) bool {
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	return strings.Contains(err.Error(), "server is closing")
}

// greetTool is the plugin's own tool: pure, no host access.
func greetTool() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "greet",
		Description: "Greet one name (the hello plugin's own tool).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "who to greet",
				},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	}
}

func greet(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(request.Params.Arguments, &args); err != nil {
		return nil, fmt.Errorf("hello: decode arguments: %w", err)
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return nil, errors.New("hello: name is required")
	}
	return textResult("Hello, " + name + "! (from the hello plugin)"), nil
}

// pingHostTool exercises the host primitive contract from the plugin
// side: one call that reports the host, one that publishes an event.
func pingHostTool() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "ping_host",
		Description: "Call the host primitives host_about and emit_event and report the result.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}
}

// pingHost calls the host MCP endpoint the host injects into every
// plugin child (CRAFT_HOST_MCP_URL with the per-plugin
// CRAFT_PLUGIN_TOKEN), using an ordinary MCP client over streamable
// HTTP. The token carries the plugin's declared permissions, which is
// why emit_event works here and secret_get would not.
func pingHost(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	session, err := connectHost(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()

	about, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "host_about"})
	if err != nil {
		return nil, fmt.Errorf("hello: host_about: %w", err)
	}
	if about.IsError {
		return nil, fmt.Errorf("hello: host_about: %s", resultText(about))
	}
	var report struct {
		Protocol    int      `json:"protocol"`
		HostVersion string   `json:"host_version"`
		Primitives  []string `json:"primitives"`
		Denied      []struct {
			Tool   string `json:"tool"`
			Reason string `json:"reason"`
		} `json:"denied"`
	}
	if err := json.Unmarshal([]byte(resultText(about)), &report); err != nil {
		return nil, fmt.Errorf("hello: decode host_about: %w", err)
	}

	emitted, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "emit_event",
		Arguments: map[string]any{
			"subject": "ping",
			"payload": map[string]any{"from": "hello"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("hello: emit_event: %w", err)
	}
	if emitted.IsError {
		return nil, fmt.Errorf("hello: emit_event: %s", resultText(emitted))
	}

	// host_about reports every primitive the host registered, plus the
	// ones this caller cannot use: no_grant means the plugin's manifest
	// does not ask for the permission, no_service means the host
	// configured no behavior for that family, disabled means the host
	// switched it off on purpose.
	var grants, missing, disabled []string
	for _, entry := range report.Denied {
		switch entry.Reason {
		case "no_grant":
			grants = append(grants, entry.Tool)
		case "disabled":
			disabled = append(disabled, entry.Tool)
		default:
			missing = append(missing, entry.Tool)
		}
	}
	callable := make([]string, 0, len(report.Primitives))
	for _, name := range report.Primitives {
		if !slices.Contains(grants, name) {
			callable = append(callable, name)
		}
	}
	summary := fmt.Sprintf(
		"host protocol %d (host version %s): %d primitives exposed, %d callable by this plugin [%s]",
		report.Protocol, report.HostVersion, len(report.Primitives),
		len(callable), strings.Join(callable, ", "))
	if len(grants) > 0 {
		summary += fmt.Sprintf("; needs a grant for [%s]", strings.Join(grants, ", "))
	}
	if len(disabled) > 0 {
		summary += fmt.Sprintf("; disabled by the host [%s]", strings.Join(disabled, ", "))
	}
	if len(missing) > 0 {
		summary += fmt.Sprintf("; %d unconfigured (no_service)", len(missing))
	}
	return textResult(summary), nil
}

// connectHost opens one MCP client session against the host endpoint.
func connectHost(ctx context.Context) (*mcpsdk.ClientSession, error) {
	endpoint := os.Getenv("CRAFT_HOST_MCP_URL")
	token := os.Getenv("CRAFT_PLUGIN_TOKEN")
	if endpoint == "" || token == "" {
		return nil, errors.New(
			"hello: no host MCP endpoint; the host must declare a host_tools service")
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    "hello",
		Version: "0.1.0",
	}, nil)
	return client.Connect(ctx, &mcpsdk.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: &bearer{token: token, base: http.DefaultTransport}},
	}, nil)
}

// bearer adds the plugin token to every request.
type bearer struct {
	token string
	base  http.RoundTripper
}

func (b *bearer) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

func textResult(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
	}
}

// resultText joins the text parts of one tool result.
func resultText(result *mcpsdk.CallToolResult) string {
	if result == nil {
		return ""
	}
	parts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		if text, ok := content.(*mcpsdk.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}
