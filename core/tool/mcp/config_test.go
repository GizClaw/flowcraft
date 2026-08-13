package mcp

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
)

func TestRegisterAddsMCPToolSourceFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "mcp"); !ok {
		t.Fatalf("factory %s/mcp missing", ResourceKind)
	}
}

func TestParseSpecRejectsMissingTransport(t *testing.T) {
	_, err := ParseSpec(json.RawMessage(`{
		"servers": [{"name": "fs", "command": "npx"}]
	}`))
	if err == nil {
		t.Fatal("ParseSpec accepted a server without transport")
	}
}

func TestParseSpecHTTPTimeout(t *testing.T) {
	spec, err := ParseSpec(json.RawMessage(`{
		"servers": [{
			"name": "remote",
			"transport": "http",
			"url": "https://mcp.example.com",
			"http_timeout": "7s"
		}]
	}`))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if spec.Servers[0].HTTPTimeout == nil || *spec.Servers[0].HTTPTimeout != "7s" {
		t.Fatalf("HTTPTimeout = %#v, want 7s", spec.Servers[0].HTTPTimeout)
	}
}

func TestParseSpecRejectsInvalidHTTPTimeout(t *testing.T) {
	_, err := ParseSpec(json.RawMessage(`{
		"servers": [{
			"name": "remote",
			"transport": "http",
			"url": "https://mcp.example.com",
			"http_timeout": "soon"
		}]
	}`))
	if err == nil {
		t.Fatal("ParseSpec accepted invalid http_timeout")
	}
}
