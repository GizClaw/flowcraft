package hostmcp

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func postMCP(t *testing.T, url, token, origin string) *http.Response {
	t.Helper()
	response, err := postJSON(url, token, origin,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// postJSON posts one MCP request and returns the transport's own
// outcome. The caller decides what a failure means, so this is safe to
// call from a goroutine that cannot report through t.Fatal.
func postJSON(url, token, origin, body string) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	return http.DefaultClient.Do(request)
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
	token, _, err := tokens.Mint(Identity{PluginID: "hello"})
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

// heldPrimitive is one registered primitive whose handler the test holds
// open, so a request that reaches it is still in flight when Close runs.
type heldPrimitive struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldPrimitive) tool() Tool {
	return Tool{
		Name: "held",
		Handler: func(context.Context, Call) (any, error) {
			h.once.Do(func() { close(h.entered) })
			<-h.release
			return map[string]any{"held": true}, nil
		},
	}
}

// callHeldBody is the request that reaches a heldPrimitive's handler: one
// MCP tool call, which is what a plugin's request looks like.
const callHeldBody = `{"jsonrpc":"2.0","id":2,"method":"tools/call",` +
	`"params":{"name":"held","arguments":{}}}`

// newHeldServer builds one started server over a registry holding one
// primitive the test can hold open, and returns it with the channels
// that control that request.
func newHeldServer(t *testing.T, opts ServerOptions) (*Server, *Tokens, *heldPrimitive, string) {
	t.Helper()
	held := &heldPrimitive{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	registry := NewRegistry()
	if err := registry.Register(held.tool()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	tokens := NewTokens()
	opts.Registry = registry
	opts.Tokens = tokens
	opts.HostVersion = "0.1.0"
	server, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	token, _, err := tokens.Mint(Identity{PluginID: "hello"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return server, tokens, held, token
}

// awaitHeld starts the request the held primitive never finishes and
// waits until it is in flight, reporting when it has been answered.
func awaitHeld(t *testing.T, server *Server, held *heldPrimitive, token string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		response, err := postJSON(server.URL(), token, "", callHeldBody)
		if err != nil {
			done <- err
			return
		}
		_ = response.Body.Close()
		done <- nil
	}()
	select {
	case <-held.entered:
	case err := <-done:
		t.Fatalf("the request finished before its handler was reached: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the primitive's handler")
	}
	return done
}

// TestServerCloseKeepsTheShutdownOutcome pins what the second Close of a
// server whose shutdown did not finish reports: the failure the first
// one saw, not a fresh nil. The flag that makes Close idempotent is set
// before the shutdown runs — Start must not be able to listen again
// while one is in flight — so the outcome is kept for whoever asks
// again, and a second answer of nil would report a shutdown that never
// finished as done.
func TestServerCloseKeepsTheShutdownOutcome(t *testing.T) {
	t.Parallel()
	server, _, held, token := newHeldServer(t, ServerOptions{
		// Short enough that the request the test holds open outlives it.
		ShutdownTimeout: 50 * time.Millisecond,
	})
	done := awaitHeld(t, server, held, token)

	first := server.Close()
	if first == nil {
		t.Fatal("Close reported success over a request that never finished")
	}
	second := server.Close()
	if second == nil {
		t.Fatal("the second Close reported a shutdown that failed as done")
	}
	if second.Error() != first.Error() {
		t.Fatalf("second Close = %v, want the first one's %v", second, first)
	}

	close(held.release)
	if err := <-done; err != nil {
		t.Fatalf("the held request: %v", err)
	}
}

// TestServerCloseWaitsForInFlightRequests pins the other half of the
// same bound: a shutdown that can finish waits for the request in
// flight, and an endpoint left to the default — ShutdownTimeout is unset
// here — waits within that default rather than within an expired
// deadline. A zero option read as a zero-length wait would turn every
// shutdown with a request in flight into a timeout.
func TestServerCloseWaitsForInFlightRequests(t *testing.T) {
	t.Parallel()
	server, _, held, token := newHeldServer(t, ServerOptions{})
	done := awaitHeld(t, server, held, token)

	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while the request was still in flight", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(held.release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the request finished")
	}
	if err := <-done; err != nil {
		t.Fatalf("the held request: %v", err)
	}
}

// TestServerRateLimitIsPerToken pins what the guide documents: a token
// over its rate limit is answered 429, and the count is the token's
// own — a second plugin's requests are not charged for the first one's.
func TestServerRateLimitIsPerToken(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	if err := Standard(registry, Services{}, "0.1.0"); err != nil {
		t.Fatalf("Standard: %v", err)
	}
	tokens := NewTokens()
	server, err := NewServer(ServerOptions{
		Registry: registry, Tokens: tokens, HostVersion: "0.1.0",
		RateLimit: 1,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	first, _, err := tokens.Mint(Identity{PluginID: "hello"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	second, _, err := tokens.Mint(Identity{PluginID: "other"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if response := postMCP(t, server.URL(), first, ""); response.StatusCode ==
		http.StatusTooManyRequests {
		t.Fatal("the first request of a token was rate limited")
	}
	if response := postMCP(t, server.URL(), first, ""); response.StatusCode !=
		http.StatusTooManyRequests {
		t.Fatalf("the request over the limit = %d, want 429", response.StatusCode)
	}
	if response := postMCP(t, server.URL(), second, ""); response.StatusCode ==
		http.StatusTooManyRequests {
		t.Fatal("a second token was charged for the first one's requests")
	}
}
