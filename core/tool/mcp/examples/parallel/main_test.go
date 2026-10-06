package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/utils"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type observedRequest struct {
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
}

func (l *requestLog) RoundTrip(req *http.Request) (*http.Response, error) {
	row := observedRequest{UA: req.UserAgent(), Auth: req.Header.Get("Authorization")}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer func() { _ = body.Close() }()
		if err := json.NewDecoder(body).Decode(&row); err != nil {
			return nil, err
		}
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
		if row.UA != "FlowCraft/parallel-mcp-example" || row.Auth != "" {
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

func fixture(t *testing.T, fail bool) []byte {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "parallel-fixture", Version: "1"}, nil)
	for _, name := range []string{"web_search", "web_fetch"} {
		server.AddTool(&mcpsdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{IsError: fail,
					Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "source: https://go.dev/doc/ Go documentation"}}}, nil
			})
	}
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	var spec map[string]any
	if err := json.Unmarshal(settings, &spec); err != nil {
		t.Fatal(err)
	}
	spec["servers"].([]any)[0].(map[string]any)["url"] = httpServer.URL
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestSearchAndFetch(t *testing.T) {
	log := &requestLog{base: http.DefaultTransport}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := run(ctx, fixture(t, false), &http.Client{Transport: log}, "FlowCraft Go MCP tool bridge", "https://go.dev/doc/", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "Go documentation") != 2 {
		t.Fatalf("expected both tool results: %s", out.String())
	}
	log.check(t, true)
}

func TestToolError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := run(ctx, fixture(t, true), nil, "query", "", &out)
	if err == nil || !strings.Contains(err.Error(), "web_search:") || out.Len() != 0 {
		t.Fatalf("expected a search error without successful output: %v, %q", err, out.String())
	}
}

func TestCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, settings, nil, "query", "", io.Discard); err == nil {
		t.Fatal("canceled context succeeded")
	}
}

// Opt in explicitly: this makes real requests and consumes anonymous limits.
func TestLiveParallel(t *testing.T) {
	if os.Getenv("FLOWCRAFT_PARALLEL_LIVE") != "1" {
		t.Skip("set FLOWCRAFT_PARALLEL_LIVE=1 to test the anonymous endpoint")
	}
	log := &requestLog{base: utils.NewRoundTripper()}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := run(ctx, settings, &http.Client{Transport: log}, "FlowCraft Go MCP tool bridge", "https://go.dev/doc/", &out); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	for range 2 {
		var result struct {
			Results []struct {
				URL      string   `json:"url"`
				Excerpts []string `json:"excerpts"`
			} `json:"results"`
		}
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		if len(result.Results) == 0 || result.Results[0].URL == "" || len(result.Results[0].Excerpts) == 0 {
			t.Fatalf("expected source URLs and excerpts: %+v", result)
		}
		t.Logf("useful output: %d sources, first URL %s, first excerpt %.160s", len(result.Results), result.Results[0].URL, result.Results[0].Excerpts[0])
	}
	log.check(t, true)
}
