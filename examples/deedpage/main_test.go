package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/tool/mcp"
	"github.com/GizClaw/flowcraft/core/utils"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolCall struct {
	Name      string
	Arguments map[string]any
}

type toolHandler func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)

type recorder struct {
	mu    sync.Mutex
	calls []toolCall
	uas   []string
	auths []string
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uas = append(r.uas, req.UserAgent())
	r.auths = append(r.auths, req.Header.Get("Authorization"))
}

func text(s string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: s}}}
}

const publishJSON = `{"ok": true, "url": "https://quiet-river-ab12.deed.page/", "slug": "quiet-river-ab12", "token": "deed_tok_fixture"}`

// fixture serves publish_site and delete_site over Streamable HTTP and
// returns a spec pointing the embedded settings at it.
func fixture(t *testing.T, override toolHandler) ([]byte, *recorder) {
	t.Helper()
	rec := &recorder{}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "deedpage-fixture", Version: "1"}, nil)
	for _, name := range []string{"publish_site", "delete_site"} {
		server.AddTool(&mcpsdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				var args map[string]any
				_ = json.Unmarshal(req.Params.Arguments, &args)
				rec.mu.Lock()
				rec.calls = append(rec.calls, toolCall{Name: req.Params.Name, Arguments: args})
				rec.mu.Unlock()
				if override != nil {
					return override(ctx, req)
				}
				if req.Params.Name == "delete_site" {
					return text(`{"ok": true, "deleted": "quiet-river-ab12"}`), nil
				}
				return text(publishJSON), nil
			})
	}
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(httpServer.Close)
	spec, err := mcp.ParseSpec(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	spec.Servers[0].URL = httpServer.URL
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, rec
}

func execute(t *testing.T, spec []byte, req request) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := run(ctx, spec, nil, req, &out)
	return out.String(), err
}

func TestPublishSendsPageAnonymously(t *testing.T) {
	spec, rec := fixture(t, nil)
	out, err := execute(t, spec, request{HTML: samplePage, Slug: "team-report", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != publishJSON {
		t.Fatalf("output: %q", out)
	}
	if len(rec.calls) != 1 || rec.calls[0].Name != "publish_site" {
		t.Fatalf("calls: %+v", rec.calls)
	}
	args := rec.calls[0].Arguments
	files, _ := args["files"].(map[string]any)
	if files["index.html"] != samplePage || args["slug"] != "team-report" || args["ttl_seconds"] != float64(3600) {
		t.Fatalf("publish arguments: %v", args)
	}
	if _, ok := args["token"]; ok {
		t.Fatalf("anonymous publish sent a token: %v", args)
	}
	for i, ua := range rec.uas {
		if ua != "FlowCraft/deedpage-mcp-example" || rec.auths[i] != "" {
			t.Fatalf("unexpected request identity: UA=%q Authorization=%q", ua, rec.auths[i])
		}
	}
}

func TestPermanentPublishOmitsTTL(t *testing.T) {
	spec, rec := fixture(t, nil)
	if _, err := execute(t, spec, request{HTML: "<p>keep</p>", Token: "deed_tok_saved"}); err != nil {
		t.Fatal(err)
	}
	args := rec.calls[0].Arguments
	if _, ok := args["ttl_seconds"]; ok || args["token"] != "deed_tok_saved" {
		t.Fatalf("publish arguments: %v", args)
	}
}

func TestDeleteUsesToken(t *testing.T) {
	spec, rec := fixture(t, nil)
	out, err := execute(t, spec, request{Delete: "quiet-river-ab12", Token: "deed_tok_saved"})
	if err != nil || !strings.Contains(out, `"deleted"`) {
		t.Fatalf("delete: %v, %q", err, out)
	}
	if len(rec.calls) != 1 || rec.calls[0].Name != "delete_site" ||
		rec.calls[0].Arguments["slug"] != "quiet-river-ab12" || rec.calls[0].Arguments["token"] != "deed_tok_saved" {
		t.Fatalf("calls: %+v", rec.calls)
	}
}

func TestInvalidRequestsFailBeforeConnecting(t *testing.T) {
	for _, req := range []request{{Delete: "quiet-river-ab12"}, {HTML: " \n"}} {
		// An unreachable spec proves validation happens before discovery.
		if _, err := execute(t, []byte(`{"servers":[]}`), req); err == nil || strings.Contains(err.Error(), "connect") {
			t.Fatalf("expected validation error for %+v: %v", req, err)
		}
	}
}

func TestToolErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *mcpsdk.CallToolResult
		want   string
	}{
		{"is-error", &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "slug_taken"}}}, "publish_site: slug_taken"},
		{"ok-false", text(`{"ok": false, "error": "too_large"}`), "too_large"},
		{"not-json", text("plain text"), "not a deed.page JSON response"},
		{"empty", &mcpsdk.CallToolResult{}, "not a deed.page JSON response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, _ := fixture(t, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) { return tc.result, nil })
			out, err := execute(t, spec, request{HTML: samplePage})
			if err == nil || !strings.Contains(err.Error(), tc.want) || out != "" {
				t.Fatalf("expected %q without output: %v, %q", tc.want, err, out)
			}
		})
	}
}

func TestCanceled(t *testing.T) {
	spec, _ := fixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, spec, nil, request{HTML: samplePage}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
}

// Opt in explicitly: this publishes a real page for ten minutes, checks it is
// served, and deletes it with the returned owner token.
func TestLiveDeedPage(t *testing.T) {
	if os.Getenv("FLOWCRAFT_DEEDPAGE_LIVE") != "1" {
		t.Skip("set FLOWCRAFT_DEEDPAGE_LIVE=1 to publish and delete a real page")
	}
	client := &http.Client{Transport: utils.NewRoundTripper()}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := run(ctx, settings, client, request{HTML: samplePage, TTL: 10 * time.Minute}, &out); err != nil {
		t.Fatal(err)
	}
	var site struct{ URL, Slug, Token string }
	if err := json.Unmarshal(out.Bytes(), &site); err != nil || site.URL == "" || site.Token == "" {
		t.Fatalf("publish result: %v, %s", err, out.String())
	}
	resp, err := http.Get(site.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", site.URL, resp.StatusCode)
	}
	if err := run(ctx, settings, client, request{Delete: site.Slug, Token: site.Token}, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Logf("published, served and deleted %s", site.URL)
}
