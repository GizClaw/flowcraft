package hostmcp

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

func postMCP(t *testing.T, url, token, origin string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestServerAuthAndOrigin(t *testing.T) {
	registry := NewRegistry()
	if err := Standard(registry, Services{}, "0.1.0"); err != nil {
		t.Fatalf("Standard: %v", err)
	}
	tokens := NewTokens()
	server, err := NewServer(ServerOptions{
		Registry: registry, Tokens: tokens, HostVersion: "0.1.0",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if server.URL() == "" {
		t.Fatal("URL is empty after Start")
	}
	if response := postMCP(t, server.URL(), "", ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", response.StatusCode)
	}
	if response := postMCP(t, server.URL(), "bogus", ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", response.StatusCode)
	}
	token, err := tokens.Mint(Identity{PluginID: "hello"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if response := postMCP(t, server.URL(), token, "https://example.com"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("origin status = %d, want 403", response.StatusCode)
	}
	if response := postMCP(t, server.URL(), token, ""); response.StatusCode == http.StatusUnauthorized ||
		response.StatusCode == http.StatusForbidden {
		t.Fatalf("authenticated status = %d, want the SDK handler to answer", response.StatusCode)
	}
}

// TestServerCloseWhileServing pins the concurrency contract of Start
// and Close: the serving goroutine owns its server reference, so Close
// may clear the field while requests are in flight and while the
// goroutine is still reaching its first statement. A field read there
// is a data race with Close, and a scheduling that loses it serves
// from a nil server.
func TestServerCloseWhileServing(t *testing.T) {
	registry := NewRegistry()
	if err := Standard(registry, Services{}, "0.1.0"); err != nil {
		t.Fatalf("Standard: %v", err)
	}
	server, err := NewServer(ServerOptions{
		Registry: registry, Tokens: NewTokens(), HostVersion: "0.1.0",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	url := server.URL()
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			// Close races these requests on purpose: the assertion is
			// that nothing panics or races, so a failed request is
			// fine.
			request, err := http.NewRequest(
				http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			if err != nil {
				t.Errorf("NewRequest: %v", err)
				return
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return
			}
			_ = response.Body.Close()
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		if err := server.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	group.Wait()
}
