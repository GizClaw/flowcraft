package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

func newTestRuntime(t *testing.T, modelName string) *runtime.Runtime {
	t.Helper()
	root := t.TempDir()
	cfg := runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Inference: runtime.InferenceConfig{
			Providers: []runtime.ProviderConfig{{Name: "p", Driver: "stub"}},
			Models:    []runtime.ModelConfig{{Provider: "p", Model: modelName}},
		},
	}
	rt, err := runtime.New(context.Background(), cfg, runtime.WithDrivers(runtime.Driver{
		Name: "stub",
		Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{Generator: &scriptedProvider{}}, nil
		},
	}))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func TestSnapshotCapturesTheRuntimeDigest(t *testing.T) {
	rt := newTestRuntime(t, "m")
	session, err := NewSession(rt)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	checkpoint := session.Snapshot()
	if checkpoint.Schema != CheckpointSchema {
		t.Errorf("schema = %d, want %d", checkpoint.Schema, CheckpointSchema)
	}
	if checkpoint.Runtime != rt.Config().Digest() {
		t.Errorf("digest = %s, want the runtime's", checkpoint.Runtime)
	}
	if checkpoint.Task.ID != session.Task().ID {
		t.Errorf("task id = %q, want %q", checkpoint.Task.ID, session.Task().ID)
	}
}

// Resuming under a different configuration is refused: everything the model was
// told about what it may do came from the configuration the checkpoint recorded.
func TestRestoreRejectsADifferentConfiguration(t *testing.T) {
	original := newTestRuntime(t, "m")
	session, err := NewSession(original)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	session.Task().History = []message.Message{message.NewTextMessage(message.RoleUser, "hi")}
	checkpoint := session.Snapshot()

	other := newTestRuntime(t, "other-model")
	_, err = Restore(other, checkpoint)
	if err == nil {
		t.Fatal("resume under a different model was accepted")
	}
	if !strings.Contains(err.Error(), "checkpoint was taken under") {
		t.Errorf("error %q does not explain the mismatch", err)
	}
}

func TestRestoreKeepsTheTask(t *testing.T) {
	rt := newTestRuntime(t, "m")
	session, err := NewSession(rt)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	session.Task().History = []message.Message{message.NewTextMessage(message.RoleUser, "hi")}
	checkpoint := session.Snapshot()

	restored, err := Restore(rt, checkpoint)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.Task().ID != session.Task().ID {
		t.Errorf("task id = %q, want %q", restored.Task().ID, session.Task().ID)
	}
	if len(restored.Task().History) != 1 {
		t.Errorf("history = %d messages, want 1", len(restored.Task().History))
	}
}

func TestRestoreRejectsAnUnknownSchema(t *testing.T) {
	rt := newTestRuntime(t, "m")
	_, err := Restore(rt, Checkpoint{Schema: CheckpointSchema + 1})
	if err == nil {
		t.Fatal("unknown schema accepted")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("error %q does not name the schema", err)
	}
}

func TestRestoreRequiresARuntime(t *testing.T) {
	if _, err := Restore(nil, Checkpoint{Schema: CheckpointSchema}); err == nil {
		t.Fatal("nil runtime accepted")
	}
}
