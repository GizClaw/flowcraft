package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestDecodeRejectsMissingTransport(t *testing.T) {
	_, err := Decode(json.RawMessage(`{
		"servers": [{"name": "fs", "command": "npx"}]
	}`))
	if err == nil {
		t.Fatal("Decode accepted a server without transport")
	}
}

// TestDecodeRejectsUnknownField pins strict decoding: a misspelled key must not
// silently become a default.
func TestDecodeRejectsUnknownField(t *testing.T) {
	_, err := Decode(json.RawMessage(`{
		"servers": [{"name": "fs", "transport": "stdio", "commnd": "npx"}]
	}`))
	if err == nil {
		t.Fatal("Decode accepted an unknown field")
	}
}

func TestDecodeRejectsEmptySpec(t *testing.T) {
	if _, err := Decode(json.RawMessage(`{"servers": []}`)); err == nil {
		t.Fatal("Decode accepted an empty server list")
	}
}

func TestDecodeRejectsDuplicateServerName(t *testing.T) {
	_, err := Decode(json.RawMessage(`{
		"servers": [
			{"name": "fs", "transport": "stdio", "command": "a"},
			{"name": "fs", "transport": "stdio", "command": "b"}
		]
	}`))
	if err == nil {
		t.Fatal("Decode accepted a duplicate server name")
	}
}

func TestDecodeHTTPTimeout(t *testing.T) {
	spec, err := Decode(json.RawMessage(`{
		"servers": [{
			"name": "remote",
			"transport": "http",
			"url": "https://mcp.example.com",
			"http_timeout": "7s"
		}]
	}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if spec.Servers[0].HTTPTimeout == nil || *spec.Servers[0].HTTPTimeout != "7s" {
		t.Fatalf("HTTPTimeout = %#v, want 7s", spec.Servers[0].HTTPTimeout)
	}
}

func TestDecodeRejectsInvalidHTTPTimeout(t *testing.T) {
	_, err := Decode(json.RawMessage(`{
		"servers": [{
			"name": "remote",
			"transport": "http",
			"url": "https://mcp.example.com",
			"http_timeout": "soon"
		}]
	}`))
	if err == nil {
		t.Fatal("Decode accepted invalid http_timeout")
	}
}

func TestDecodeRequired(t *testing.T) {
	spec, err := Decode(json.RawMessage(`{
		"servers": [{
			"name": "db",
			"transport": "stdio",
			"command": "mcp-db",
			"required": true
		}]
	}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if spec.Servers[0].Required == nil || !*spec.Servers[0].Required {
		t.Fatal("Required not parsed from the spec")
	}

	cfg := &serverConfig{}
	opts, err := spec.Servers[0].options()
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	for _, opt := range opts {
		opt(cfg)
	}
	if !cfg.required {
		t.Fatal("WithRequired not wired from ServerSpec.options")
	}
}

func TestDecodeLiveness(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{raw: `"30s"`, want: 30 * time.Second},
		{raw: `"off"`, want: 0},
	} {
		spec, err := Decode(json.RawMessage(`{
			"servers": [{
				"name": "fs",
				"transport": "stdio",
				"command": "mcp-fs",
				"liveness": ` + tc.raw + `
			}]
		}`))
		if err != nil {
			t.Fatalf("Decode(liveness=%s): %v", tc.raw, err)
		}
		cfg := &serverConfig{}
		opts, err := spec.Servers[0].options()
		if err != nil {
			t.Fatalf("options(liveness=%s): %v", tc.raw, err)
		}
		for _, opt := range opts {
			opt(cfg)
		}
		if cfg.liveness == nil {
			t.Fatalf("liveness %s: WithServerLiveness not wired from ServerSpec.options", tc.raw)
		}
		if *cfg.liveness != tc.want {
			t.Fatalf("liveness %s = %v, want %v", tc.raw, *cfg.liveness, tc.want)
		}
	}
}

func TestDecodeRejectsInvalidLiveness(t *testing.T) {
	for _, raw := range []string{`"soon"`, `"0s"`, `"-5s"`, `""`} {
		_, err := Decode(json.RawMessage(`{
			"servers": [{
				"name": "fs",
				"transport": "stdio",
				"command": "mcp-fs",
				"liveness": ` + raw + `
			}]
		}`))
		if err == nil {
			t.Fatalf("Decode accepted invalid liveness %s", raw)
		}
	}
}

// TestConnectRejectsInvalidSpec keeps a bad declaration from opening anything.
func TestConnectRejectsInvalidSpec(t *testing.T) {
	_, err := Connect(context.Background(), Spec{})
	if err == nil {
		t.Fatal("Connect accepted an empty spec")
	}
}
