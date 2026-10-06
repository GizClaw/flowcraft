// Command parallel searches the web through FlowCraft's HTTP MCP bridge.
package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/mcp"
)

//go:embed servers.json
var settings []byte

func main() {
	query := flag.String("query", "FlowCraft Go MCP tool bridge", "search query (also used as the search objective)")
	fetch := flag.String("fetch", "", "optional HTTP(S) URL to fetch after searching")
	timeout := flag.Duration("timeout", 90*time.Second, "total timeout including discovery and tool calls")
	flag.Parse()
	if strings.TrimSpace(*query) == "" || *timeout <= 0 || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "provide a nonempty -query, a positive -timeout, and no positional arguments")
		os.Exit(2)
	}
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	if err := run(ctx, settings, nil, *query, *fetch, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run uses the same declarative factory, registry, and executor as a host.
// The client parameter lets tests observe requests without changing the spec.
func run(ctx context.Context, spec []byte, client *http.Client, query, fetchURL string, out io.Writer) error {
	built, err := mcp.NewFactory(mcp.WithHTTPClient(client)).New(ctx, resource.Input{Settings: spec})
	if err != nil {
		return err
	}
	source := built.(*mcp.Source)
	defer func() { _ = source.Close() }() // cancels background retries and releases the session
	registry, err := tool.NewRegistry([]tool.Source{source})
	if err != nil {
		return err
	}
	defer func() { _ = registry.Close() }()
	source.Attach(registry)
	if err := source.WaitReady(ctx, "parallel", 0); err != nil {
		return fmt.Errorf("connect Parallel MCP: %w", err)
	}

	executor := tool.NewExecutor(registry)
	// One invocation is one conversation; search and fetch share its identifier.
	var session [16]byte
	if _, err := rand.Read(session[:]); err != nil {
		return err
	}
	sessionID := fmt.Sprintf("%x", session)
	call := func(name string, args map[string]any) error {
		args["session_id"] = sessionID
		encoded, err := json.Marshal(args)
		if err != nil {
			return err
		}
		result := executor.Execute(ctx, message.ToolCall{ID: name, Name: "parallel__" + name, Arguments: encoded})
		if result.IsError {
			return fmt.Errorf("%s: %s", name, result.Content.Text())
		}
		_, err = fmt.Fprintln(out, result.Content.Text())
		return err
	}
	if err := call("web_search", map[string]any{"objective": query, "search_queries": []string{query}}); err != nil {
		return err
	}
	if fetchURL != "" {
		return call("web_fetch", map[string]any{"urls": []string{fetchURL}})
	}
	return nil
}
