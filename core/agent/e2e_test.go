package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
)

const e2eConfig = `
version: 1
name: e2e
sandbox:
  backend: local
  workdir: ${E2E_ROOT}
  writes: ["${E2E_ROOT}"]
files:
  roots: ["${E2E_ROOT}"]
inference:
  providers:
    - name: p
      driver: stub
  models:
    - provider: p
      model: m
tools:
  enable: [files]
`

// One deployment, assembled from a document, driven through one Task, observed
// as one trace. This is the first test where configuration, assembly, the loop
// and telemetry all have to agree.
func TestDeploymentRunsOneTask(t *testing.T) {
	recorder := telemetrytest.New(t)
	root := t.TempDir()
	t.Setenv("E2E_ROOT", root)

	cfg, _, err := runtime.LoadLayers(context.Background(), []runtime.Layer{{
		Name: "e2e.yaml", Bytes: []byte(e2eConfig),
	}})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}

	path := filepath.Join(root, "doc.txt")
	if err := writeFixture(t, root, path); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	call, err := message.NewToolCall("call-1", "read_file", map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	provider := &scriptedProvider{
		steps: []inference.GenerateResponse{toolCallResponse(t, call), textResponse("done")},
	}
	rt, err := runtime.New(context.Background(), cfg, runtime.WithDrivers(runtime.Driver{
		Name: "stub",
		Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{Generator: provider}, nil
		},
	}))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	session, err := NewSession(rt)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	var content string
	for event, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "what does doc.txt say?"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if result, ok := event.(ToolResult); ok {
			var payload map[string]any
			if err := json.Unmarshal([]byte(result.Result.Content.Text()), &payload); err == nil {
				if text, ok := payload["content"].(string); ok {
					content = text
				}
			}
		}
	}
	if content == "" {
		t.Fatal("the tool result did not carry the file content")
	}
	if session.Task().State != StateCompleted {
		t.Errorf("state = %s, want completed", session.Task().State)
	}

	// The trace: one root span for the Task, and the tool call under it.
	turn := recorder.RequireNamed(t, "agent.run")
	telemetrytest.RequireAttrString(t, turn, telemetry.AttrRuntimeID, rt.ID())
	telemetrytest.RequireAttrString(t, turn, telemetry.AttrTaskID, session.Task().ID)
	telemetrytest.RequireAttrString(t, turn, telemetry.AttrLLMModel, "m")

	tool := recorder.RequireNamed(t, "tool.execute")
	telemetrytest.RequireAttrString(t, tool, telemetry.AttrToolName, "read_file")
	if tool.Parent().SpanID() != turn.SpanContext().SpanID() {
		t.Errorf("tool span parent = %s, want the agent span %s",
			tool.Parent().SpanID(), turn.SpanContext().SpanID())
	}
	if turn.Parent().IsValid() {
		t.Error("the agent span should be the root of this trace")
	}
}

func writeFixture(t *testing.T, root, path string) error {
	t.Helper()
	tree, err := fsx.Open(fsx.WithWritable(root))
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	return tree.Write(context.Background(), path, []byte("hello e2e"))
}
