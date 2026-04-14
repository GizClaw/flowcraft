package plugin

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPluginTypeConstants(t *testing.T) {
	cases := []struct {
		pt   PluginType
		want string
	}{
		{TypeModel, "model"},
		{TypeTool, "tool"},
		{TypeNode, "node"},
		{TypeStrategy, "agent_strategy"},
		{TypeData, "data_source"},
	}
	for _, tc := range cases {
		if string(tc.pt) != tc.want {
			t.Errorf("PluginType = %q, want %q", tc.pt, tc.want)
		}
	}
}

func TestPluginStatusConstants(t *testing.T) {
	cases := []struct {
		ps   PluginStatus
		want string
	}{
		{StatusInstalled, "installed"},
		{StatusActive, "active"},
		{StatusInactive, "inactive"},
		{StatusError, "error"},
	}
	for _, tc := range cases {
		if string(tc.ps) != tc.want {
			t.Errorf("PluginStatus = %q, want %q", tc.ps, tc.want)
		}
	}
}

func TestPluginInfoJSON(t *testing.T) {
	info := PluginInfo{
		ID:      "test-plugin",
		Name:    "Test Plugin",
		Version: "1.0.0",
		Type:    TypeTool,
		Builtin: true,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded PluginInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != info.ID || decoded.Name != info.Name || decoded.Version != info.Version {
		t.Errorf("round-trip mismatch: got %+v", decoded)
	}
	if decoded.Type != TypeTool {
		t.Errorf("type = %q, want %q", decoded.Type, TypeTool)
	}
	if !decoded.Builtin {
		t.Error("builtin should be true")
	}
}

func TestPluginInfoOmitEmpty(t *testing.T) {
	info := PluginInfo{
		ID:      "minimal",
		Name:    "Minimal",
		Version: "0.1.0",
		Type:    TypeNode,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	raw := string(data)
	for _, field := range []string{"description", "author", "icon", "homepage"} {
		if containsKey(raw, field) {
			t.Errorf("expected %q to be omitted, but found in JSON", field)
		}
	}
}

func TestToolSpecJSON(t *testing.T) {
	spec := ToolSpec{
		Name:        "search",
		Description: "Search the web",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ToolSpec
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Name != "search" || decoded.Description != "Search the web" {
		t.Errorf("round-trip mismatch: %+v", decoded)
	}
	if decoded.InputSchema == nil {
		t.Error("input_schema should not be nil")
	}
}

func TestInstalledPluginJSON(t *testing.T) {
	ip := InstalledPlugin{
		Info: PluginInfo{
			ID:        "ip-1",
			Name:      "Installed",
			Version:   "2.0.0",
			Type:      TypeModel,
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		Status: StatusActive,
		Config: map[string]any{"api_key": "sk-xxx"},
	}

	data, err := json.Marshal(ip)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded InstalledPlugin
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Status != StatusActive {
		t.Errorf("status = %q, want %q", decoded.Status, StatusActive)
	}
	if decoded.Info.ID != "ip-1" {
		t.Errorf("info.id = %q, want %q", decoded.Info.ID, "ip-1")
	}
	if decoded.Config["api_key"] != "sk-xxx" {
		t.Error("config api_key mismatch")
	}
}

func TestInstalledPluginErrorField(t *testing.T) {
	ip := InstalledPlugin{
		Info:   PluginInfo{ID: "err-plugin", Name: "Err", Version: "0.0.1", Type: TypeTool},
		Status: StatusError,
		Error:  "connection refused",
	}

	data, err := json.Marshal(ip)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded InstalledPlugin
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Error != "connection refused" {
		t.Errorf("error = %q, want %q", decoded.Error, "connection refused")
	}
}

func containsKey(jsonStr, key string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
