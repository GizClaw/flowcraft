package a2aprotocol

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Config{
		Name: "storyteller",
		Settings: settingsJSON(map[string]any{"card": map[string]any{
			"version": "1.0.0", "url": "https://example.test",
		}}),
		Agent: tableFor(t, &completedRunner{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

// The card is what a client discovers first, so it must be served where the
// convention expects and must say what the deployment declared.
func TestServerServesTheCard(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cardPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("content type = %q, want json", ct)
	}
	var card map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("card is not JSON: %v", err)
	}
	if card["name"] != "storyteller" {
		t.Errorf("name = %v", card["name"])
	}
	capabilities, _ := card["capabilities"].(map[string]any)
	if capabilities["streaming"] != true {
		t.Errorf("streaming = %v, want true", capabilities["streaming"])
	}
}

// The RPC endpoint is mounted, so a request reaches the SDK's handler rather
// than a 404.
func TestServerReportsItsCard(t *testing.T) {
	if got := testServer(t).Card().Name; got != "storyteller" {
		t.Errorf("card name = %q, want the deployment's", got)
	}
}

func TestServerMountsTheRPCEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"jsonrpc":"2.0","id":"1","method":"no/such"}`)
	testServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, rpcPath, body))
	if rec.Code == http.StatusNotFound {
		t.Fatal("the RPC endpoint is not mounted")
	}
	if rec.Body.Len() == 0 {
		t.Error("the RPC endpoint answered with an empty body")
	}
}

func TestNewRequiresAnAgent(t *testing.T) {
	_, err := New(Config{
		Name: "x",
		Settings: settingsJSON(map[string]any{"card": map[string]any{
			"version": "1", "url": "https://x",
		}}),
	})
	if err == nil {
		t.Fatal("a server without an agent was accepted")
	}
}
