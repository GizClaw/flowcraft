package craft

import (
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/craft/hostmcp"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// newTestHostMCP opens a host MCP endpoint with the standard primitives,
// started, so a test can call it the way a plugin process does.
func newTestHostMCP(t *testing.T) *hostMCPState {
	t.Helper()
	registry := hostmcp.NewRegistry()
	if err := hostmcp.Standard(registry, hostmcp.Services{}, "0.1.0"); err != nil {
		t.Fatalf("Standard: %v", err)
	}
	tokens := hostmcp.NewTokens()
	server, err := hostmcp.NewServer(hostmcp.ServerOptions{
		Registry:    registry,
		Tokens:      tokens,
		HostVersion: "0.1.0",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return &hostMCPState{
		server: server,
		tokens: tokens,
		byID:   map[string]mintedTokens{},
	}
}

// callHostMCP posts one MCP initialize with a token and returns the
// status the endpoint answered with.
func callHostMCP(t *testing.T, url, token string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

// TestMintedTokensBelongToOneProcessGeneration pins what a plugin token
// is accounted against: the process it was minted for. The next process
// of the same plugin takes the previous one's tokens away and drops the
// SDK server that was built from its grants — including when the
// previous process never made it into the tool set, the one replacement
// no stop notification explains.
func TestMintedTokensBelongToOneProcessGeneration(t *testing.T) {
	t.Parallel()
	state := newTestHostMCP(t)
	entry := plugin.Entry{
		ID: "hello",
		Manifest: plugin.Manifest{
			ID:          "hello",
			Version:     "0.1.0",
			Permissions: []string{"mcp:provide", "secrets:auth"},
		},
	}
	first, err := state.mint(entry, 1)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// The process used its token, which is what puts an SDK server in
	// the endpoint's cache; the tools that server exposes are the ones
	// the grants of this process allow.
	if status := callHostMCP(t, state.server.URL(), first); status != http.StatusOK {
		t.Fatalf("the process's own token answered %d, want 200", status)
	}
	if !state.server.Cached("hello") {
		t.Fatal("the endpoint cached no server for the calling plugin")
	}

	// The process is replaced by one whose manifest asks for less.
	smaller := entry
	smaller.Manifest.Version = "0.2.0"
	smaller.Manifest.Permissions = []string{"mcp:provide"}
	second, err := state.mint(smaller, 2)
	if err != nil {
		t.Fatalf("mint for the new generation: %v", err)
	}
	if status := callHostMCP(t, state.server.URL(), first); status != http.StatusUnauthorized {
		t.Fatalf("the replaced process's token answered %d, want 401", status)
	}
	if state.server.Cached("hello") {
		t.Fatal("the SDK server of the replaced process is still cached")
	}
	if status := callHostMCP(t, state.server.URL(), second); status != http.StatusOK {
		t.Fatalf("the new process's token answered %d, want 200", status)
	}
	identity, _, ok := state.tokens.Lookup(second)
	if !ok || identity.Grants.Has("secrets:auth") {
		t.Fatalf("the new token's grants = %+v, want the new manifest's", identity)
	}
	// The tokens of one generation are what a stop revokes; nothing of
	// the replaced generation is left to revoke.
	state.revokePlugin("hello")
	if _, _, ok := state.tokens.Lookup(second); ok {
		t.Fatal("the token outlived the plugin")
	}
	if len(state.byID) != 0 {
		t.Fatalf("tokens recorded after the revoke = %v", state.byID)
	}
}
