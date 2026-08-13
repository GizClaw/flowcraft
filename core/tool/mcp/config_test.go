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
