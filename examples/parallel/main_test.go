package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/tool/mcp"
	"github.com/GizClaw/flowcraft/core/utils"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type observedRequest struct {
	Host   string
	Method string
	UA     string
	Auth   string
	Params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
}

type requestLog struct {
	base http.RoundTripper
	mu   sync.Mutex
	rows []observedRequest
	host string
}

func (l *requestLog) RoundTrip(req *http.Request) (*http.Response, error) {
	row := observedRequest{Host: req.URL.Host, UA: req.UserAgent(), Auth: req.Header.Get("Authorization")}
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
		_ = json.Unmarshal(body, &row) // GET/DELETE/notifications need not carry JSON-RPC.
	}
	l.mu.Lock()
	l.rows = append(l.rows, row)
	l.mu.Unlock()
	return l.base.RoundTrip(req)
}

func (l *requestLog) check(t *testing.T, wantFetch bool) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	methods := map[string]int{}
	calls := map[string]int{}
	var sessionID string
	for _, row := range l.rows {
		if row.Host != l.host || row.UA != "FlowCraft/parallel-mcp-example" || row.Auth != "" {
			t.Fatalf("unexpected request identity: UA=%q Authorization=%q", row.UA, row.Auth)
		}
		methods[row.Method]++
		if row.Method == "tools/call" {
			calls[row.Params.Name]++
			id, _ := row.Params.Arguments["session_id"].(string)
			if len(id) < 32 || (sessionID != "" && id != sessionID) {
				t.Fatalf("missing or inconsistent session_id: %q", id)
			}
			sessionID = id
			if row.Params.Name == "web_search" {
				if row.Params.Arguments["objective"] != "FlowCraft Go MCP tool bridge" {
					t.Fatalf("search objective: %v", row.Params.Arguments)
				}
				queries, ok := row.Params.Arguments["search_queries"].([]any)
				if !ok || len(queries) != 1 || queries[0] != "FlowCraft Go MCP tool bridge" {
					t.Fatalf("search queries: %v", row.Params.Arguments)
				}
			}
			if row.Params.Name == "web_fetch" {
				urls, ok := row.Params.Arguments["urls"].([]any)
				if !ok || len(urls) != 1 || urls[0] != "https://go.dev/doc/" {
					t.Fatalf("fetch URLs: %v", row.Params.Arguments)
				}
			}
		}
	}
	if methods["initialize"] == 0 || methods["tools/list"] == 0 || calls["web_search"] != 1 {
		t.Fatalf("missing discovery or search: methods=%v calls=%v", methods, calls)
	}
	if wantFetch && calls["web_fetch"] != 1 {
		t.Fatalf("missing fetch: %v", calls)
	}
	t.Logf("observed anonymous discovery and calls: methods=%v calls=%v", methods, calls)
}

// Distinct payloads mirror the documented JSON text envelope without depending
// on live rankings or consuming anonymous limits.
const searchJSON = `{
  "results": [{"url": "https://go.dev/learn/", "title": "Learn Go",
    "excerpts": ["Search excerpt: documentation for the Go language."]}]
}`
const fetchJSON = `{
  "results": [{"url": "https://go.dev/doc/", "title": "Go documentation",
    "excerpts": ["Fetch excerpt: getting started with Go and its standard library."]}]
}`

type toolHandler func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)

func fixture(t *testing.T, override toolHandler) []byte {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "parallel-fixture", Version: "1"}, nil)
	for _, name := range []string{"web_search", "web_fetch"} {
		server.AddTool(&mcpsdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				if override != nil {
					return override(ctx, req)
				}
				return fixtureResult(req.Params.Name), nil
			})
	}
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return localSpec(t, httpServer.URL)
}

func fixtureResult(name string) *mcpsdk.CallToolResult {
	text := searchJSON
	if name == "web_fetch" {
		text = fetchJSON
	}
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}
}

func localSpec(t *testing.T, endpoint string) []byte {
	t.Helper()
	spec, err := mcp.ParseSpec(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Servers) != 1 {
		t.Fatalf("offline fixture requires exactly one server, got %d", len(spec.Servers))
	}
	spec.Servers[0].URL = endpoint
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func checkOutput(t *testing.T, out *bytes.Buffer, exact bool, count int) {
	t.Helper()
	decoder := json.NewDecoder(out)
	for i := range count {
		var result struct {
			Results []struct {
				URL      string   `json:"url"`
				Excerpts []string `json:"excerpts"`
			} `json:"results"`
		}
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		if len(result.Results) == 0 || result.Results[0].URL == "" || len(result.Results[0].Excerpts) == 0 || result.Results[0].Excerpts[0] == "" {
			t.Fatalf("expected source URLs and excerpts: %+v", result)
		}
		if exact {
			url := "https://go.dev/learn/"
			excerpt := "Search excerpt: documentation for the Go language."
			if i == 1 {
				url = "https://go.dev/doc/"
				excerpt = "Fetch excerpt: getting started with Go and its standard library."
			}
			if len(result.Results) != 1 || result.Results[0].URL != url || len(result.Results[0].Excerpts) != 1 || result.Results[0].Excerpts[0] != excerpt {
				t.Fatalf("document %d: %+v", i, result)
			}
		}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("expected end of output, got %v, %v", extra, err)
	}
}

func TestSearchAndFetch(t *testing.T) {
	spec := fixture(t, nil)
	var parsed mcp.Spec
	if err := json.Unmarshal(spec, &parsed); err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(parsed.Servers[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	log := &requestLog{base: utils.NewRoundTripper(utils.WithoutRetry()), host: endpoint.Host}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := run(ctx, spec, &http.Client{Transport: log}, "FlowCraft Go MCP tool bridge", "https://go.dev/doc/", &out); err != nil {
		t.Fatal(err)
	}
	checkOutput(t, &out, true, 2)
	log.check(t, true)
}

// Exercise the embedded timeout and hardened client, as used by the CLI.
func TestDefaultClientSearchAndFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := run(ctx, fixture(t, nil), nil, "query", "https://go.dev/doc/", &out); err != nil {
		t.Fatal(err)
	}
	checkOutput(t, &out, true, 2)
}

func TestToolError(t *testing.T) {
	for _, failTool := range []string{"web_search", "web_fetch"} {
		t.Run(failTool, func(t *testing.T) {
			spec := fixture(t, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				if req.Params.Name == failTool {
					return &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "fixture failure"}}}, nil
				}
				return fixtureResult(req.Params.Name), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out bytes.Buffer
			err := run(ctx, spec, nil, "query", "https://go.dev/doc/", &out)
			if err == nil || !strings.Contains(err.Error(), failTool+": fixture failure") {
				t.Fatalf("expected %s error: %v", failTool, err)
			}
			if failTool == "web_search" {
				if out.Len() != 0 {
					t.Fatalf("unexpected output: %q", out.String())
				}
			} else {
				checkOutput(t, &out, true, 1)
			}
		})
	}
}

func TestNoPrintableText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *mcpsdk.CallToolResult
	}{
		{"empty", &mcpsdk.CallToolResult{}},
		{"whitespace", &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: " \n"}}}},
		{"image", &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte("fixture")}}}},
		{"structured", &mcpsdk.CallToolResult{StructuredContent: map[string]any{"results": []any{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := fixture(t, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) { return tc.result, nil })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out bytes.Buffer
			err := run(ctx, spec, nil, "query", "", &out)
			if err == nil || !strings.Contains(err.Error(), "web_search: result contains no printable text") || out.Len() != 0 {
				t.Fatalf("expected no-text error without output: %v, %q", err, out.String())
			}
		})
	}
}

func TestCanceled(t *testing.T) {
	spec := fixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, spec, nil, "query", "", io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
}

func TestDiscoveryTimeout(t *testing.T) {
	entered := make(chan struct{}, 1)
	sdkServer := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "stalled", Version: "1"}, nil)
	sdkServer.AddTool(&mcpsdk.Tool{Name: "web_search", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return fixtureResult("web_search"), nil
		})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return sdkServer }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var request struct{ Method string }
		if req.Body != nil {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("read discovery request: %v", err)
				return
			}
			_ = req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(body))
			_ = json.Unmarshal(body, &request)
		}
		if request.Method == "tools/list" {
			entered <- struct{}{}
			<-req.Context().Done()
			return
		}
		handler.ServeHTTP(w, req)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := run(ctx, localSpec(t, server.URL), nil, "query", "", &out)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "connect Parallel MCP:") || out.Len() != 0 {
		t.Fatalf("expected bounded discovery error: %v, %q", err, out.String())
	}
	select {
	case <-entered:
	default:
		t.Fatal("never reached tool discovery")
	}
}

func TestMidFlightCanceled(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	spec := fixture(t, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return fixtureResult("web_search"), nil
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- run(ctx, spec, nil, "query", "", &out) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("search never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || out.Len() != 0 {
			t.Fatalf("expected canceled search without output: %v, %q", err, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled call did not stop")
	}
}

// Opt in explicitly: this makes real requests and consumes anonymous limits.
func TestLiveParallel(t *testing.T) {
	if os.Getenv("FLOWCRAFT_PARALLEL_LIVE") != "1" {
		t.Skip("set FLOWCRAFT_PARALLEL_LIVE=1 to test the anonymous endpoint")
	}
	log := &requestLog{base: utils.NewRoundTripper(), host: "search.parallel.ai"}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := run(ctx, settings, &http.Client{Transport: log}, "FlowCraft Go MCP tool bridge", "https://go.dev/doc/", &out); err != nil {
		t.Fatal(err)
	}
	checkOutput(t, &out, false, 2)
	log.check(t, true)
}
