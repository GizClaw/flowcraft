package plugin

import (
	"encoding/json"
	"testing"
)

func TestHandshakeRequestJSON(t *testing.T) {
	req := HandshakeRequest{
		HostVersion: "1.0.0",
		ProtocolVer: 1,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded HandshakeRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.HostVersion != "1.0.0" || decoded.ProtocolVer != 1 {
		t.Errorf("mismatch: %+v", decoded)
	}
}

func TestHandshakeResponseJSON(t *testing.T) {
	resp := HandshakeResponse{
		PluginInfo: PluginInfo{
			ID:      "resp-plugin",
			Name:    "Resp",
			Version: "2.0.0",
			Type:    TypeNode,
		},
		ProtocolVer: 1,
		Tools: []ToolSpec{
			{Name: "t1", Description: "tool one"},
		},
		Nodes: []NodeSpec{
			{Type: "custom", Schema: map[string]any{"max": 10}},
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded HandshakeResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.PluginInfo.ID != "resp-plugin" {
		t.Errorf("plugin id = %q", decoded.PluginInfo.ID)
	}
	if len(decoded.Tools) != 1 || decoded.Tools[0].Name != "t1" {
		t.Errorf("tools = %+v", decoded.Tools)
	}
	if len(decoded.Nodes) != 1 || decoded.Nodes[0].Type != "custom" {
		t.Errorf("nodes = %+v", decoded.Nodes)
	}
}

func TestHandshakeResponseOmitEmpty(t *testing.T) {
	resp := HandshakeResponse{
		PluginInfo:  PluginInfo{ID: "min", Name: "M", Version: "0.1.0", Type: TypeTool},
		ProtocolVer: 1,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	_ = json.Unmarshal(data, &raw)

	if _, ok := raw["tools"]; ok {
		t.Error("expected tools to be omitted when empty")
	}
	if _, ok := raw["nodes"]; ok {
		t.Error("expected nodes to be omitted when empty")
	}
}

func TestNodeSpecJSON(t *testing.T) {
	spec := NodeSpec{
		Type:   "transformer",
		Schema: map[string]any{"inputs": []string{"text"}, "outputs": []string{"result"}},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded NodeSpec
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "transformer" {
		t.Errorf("type = %q", decoded.Type)
	}
	if decoded.Schema == nil {
		t.Error("schema should not be nil")
	}
}

func TestNodeSpecOmitEmptySchema(t *testing.T) {
	spec := NodeSpec{Type: "simple"}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	_ = json.Unmarshal(data, &raw)

	if _, ok := raw["schema"]; ok {
		t.Error("expected schema to be omitted when nil")
	}
}
