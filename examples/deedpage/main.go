// Command deedpage publishes an HTML page through FlowCraft's HTTP MCP bridge.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
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

const samplePage = `<!doctype html><meta charset="utf-8"><title>FlowCraft report</title>` +
	`<h1>FlowCraft report</h1><p>Published through FlowCraft's MCP tool bridge.</p>`

// request is one publish or delete. A non-empty Delete selects deletion.
type request struct {
	HTML   string
	Slug   string
	TTL    time.Duration
	Delete string
	Token  string
}

func main() {
	file := flag.String("file", "", "HTML file to publish as index.html (default: a sample page)")
	slug := flag.String("slug", "", "optional subdomain to claim, e.g. my-team-report")
	ttl := flag.Duration("ttl", time.Hour, "time until the page expires; 0 keeps it")
	del := flag.String("delete", "", "delete this slug using the owner token in DEEDPAGE_TOKEN")
	timeout := flag.Duration("timeout", 90*time.Second, "total timeout including discovery and the tool call")
	flag.Parse()
	if *timeout <= 0 || *ttl < 0 || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "provide a positive -timeout, a non-negative -ttl, and no positional arguments")
		os.Exit(2)
	}
	req := request{Slug: *slug, TTL: *ttl, Delete: *del, Token: os.Getenv("DEEDPAGE_TOKEN"), HTML: samplePage}
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		req.HTML = string(data)
	}
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	if err := run(ctx, settings, nil, req, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// arguments validates req before any connection is opened.
func arguments(req request) (string, map[string]any, error) {
	if req.Delete != "" {
		if strings.TrimSpace(req.Token) == "" {
			return "", nil, errors.New("delete needs the owner token from the first publish in DEEDPAGE_TOKEN")
		}
		return "delete_site", map[string]any{"slug": req.Delete, "token": req.Token}, nil
	}
	if strings.TrimSpace(req.HTML) == "" {
		return "", nil, errors.New("the page to publish is empty")
	}
	args := map[string]any{"files": map[string]string{"index.html": req.HTML}}
	if req.Slug != "" {
		args["slug"] = req.Slug
	}
	if req.TTL > 0 {
		args["ttl_seconds"] = int64(req.TTL / time.Second)
	}
	if req.Token != "" {
		args["token"] = req.Token // updates a slug this owner already holds
	}
	return "publish_site", args, nil
}

// run uses the same declarative factory, registry, and executor as a host.
// The client parameter lets tests observe requests without changing the spec.
func run(ctx context.Context, spec []byte, client *http.Client, req request, out io.Writer) error {
	name, args, err := arguments(req)
	if err != nil {
		return err
	}
	built, err := mcp.NewFactory(mcp.WithHTTPClient(client)).New(ctx, resource.Input{Settings: spec})
	if err != nil {
		return err
	}
	source, ok := built.(*mcp.Source)
	if !ok {
		return fmt.Errorf("MCP factory returned %T, expected *mcp.Source", built)
	}
	defer func() { _ = source.Close() }()
	registry, err := tool.NewRegistry([]tool.Source{source})
	if err != nil {
		return err
	}
	defer func() { _ = registry.Close() }()
	// Pinned core v0.4.9 attaches sources from the assembly, not from
	// NewRegistry, so the example publishes explicitly.
	source.Attach(registry)
	if err := source.WaitReady(ctx, "deedpage", 0); err != nil {
		return fmt.Errorf("connect deed.page MCP: %w", err)
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	result := tool.NewExecutor(registry).Execute(ctx, message.ToolCall{ID: name, Name: "deedpage__" + name, Arguments: encoded})
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	text := result.Content.Text()
	if result.IsError {
		return fmt.Errorf("%s: %s", name, text)
	}
	var status struct {
		OK *bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(text), &status); err != nil || status.OK == nil {
		return fmt.Errorf("%s: result is not a deed.page JSON response: %q", name, text)
	}
	if !*status.OK {
		return fmt.Errorf("%s: %s", name, text)
	}
	_, err = fmt.Fprintln(out, text)
	return err
}
