package delegation_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func findTool(t *testing.T, service *delegation.Service, name string) tool.Tool {
	t.Helper()
	for _, candidate := range delegation.Tools(service).Tools() {
		if candidate.Definition().Name == name {
			return candidate
		}
	}
	t.Fatalf("tool %q is not exposed", name)
	return nil
}

func TestDelegateToolReturnsTheChildAnswer(t *testing.T) {
	sessions, _ := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{Targets: []string{"worker"}})
	content, err := findTool(t, service, "delegate").Execute(
		parentCtx("call-1"), `{"target":"worker","input":"do it"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if content.Text() != "done" {
		t.Fatalf("content = %q", content.Text())
	}
}

func TestTargetsToolListsTheAllowlist(t *testing.T) {
	sessions, _ := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{Targets: []string{"worker"}})
	content, err := findTool(t, service, "delegation_targets").Execute(
		parentCtx("call-1"), `{}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var raw []byte
	for _, part := range content.Parts {
		if data, ok := part.(message.DataPart); ok {
			raw = data.Value
			break
		}
	}
	if len(raw) == 0 {
		t.Fatalf("no data part in %+v", content.Parts)
	}
	var body struct {
		Targets []string `json:"targets"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Targets) != 1 || body.Targets[0] != "worker" {
		t.Fatalf("targets = %v", body.Targets)
	}
}

// The tool source is a plain tool.Source: the host can inject it without
// knowing the delegation package.
var _ tool.Source = delegation.Tools(nil)
var _ = context.Background
