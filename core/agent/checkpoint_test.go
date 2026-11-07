package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

func newTestRuntime(t testing.TB, modelName string) *runtime.Runtime {
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
	seedHistory(t, session, []message.Message{message.NewTextMessage(message.RoleUser, "hi")})
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
	seedHistory(t, session, []message.Message{message.NewTextMessage(message.RoleUser, "hi")})
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

// A deployment that is more than its runtime config records the wider identity;
// the runtime's own digest stays the default.
func TestDigestOverrideIsRecorded(t *testing.T) {
	rt := newTestRuntime(t, "m")
	session, err := NewSession(rt, WithDigest("sha256:custom"))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	got := session.Snapshot().Runtime
	if got == rt.Config().Digest() || got == "sha256:custom" {
		t.Errorf("digest = %s, want the runtime digest extended by the override", got)
	}
	if again := session.Snapshot().Runtime; again != got {
		t.Errorf("digest is not stable: %s != %s", again, got)
	}
}

// Restore compares against the same identity: with the override it accepts, and
// without it the runtime digest no longer matches what the checkpoint recorded.
func TestRestoreHonoursTheDigestOverride(t *testing.T) {
	rt := newTestRuntime(t, "m")
	session, err := NewSession(rt, WithDigest("sha256:custom"))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	checkpoint := session.Snapshot()

	if _, err := Restore(rt, checkpoint, WithDigest("sha256:custom")); err != nil {
		t.Fatalf("Restore with the override: %v", err)
	}
	if _, err := Restore(rt, checkpoint); err == nil {
		t.Fatal("restore without the override was accepted")
	}
}

// seedHistory installs a transcript directly, under the session lock: Task()
// returns a snapshot, so mutating its History would be a silent no-op.
func seedHistory(t *testing.T, session *Session, history []message.Message) {
	t.Helper()
	session.mu.Lock()
	session.task.History = history
	session.mu.Unlock()
}

// The digest is a constant per session; recomputing it per snapshot would
// re-encode the configuration on every round boundary.
func TestResumeDigestAllocatesNothing(t *testing.T) {
	rt := newTestRuntime(t, "m")
	session, err := NewSession(rt, WithDigest("sha256:override"))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if got := testing.AllocsPerRun(100, func() { _ = session.resumeDigest() }); got != 0 {
		t.Fatalf("resumeDigest allocates %.0f times, want 0", got)
	}
}

func BenchmarkResumeDigest(b *testing.B) {
	rt := newTestRuntime(b, "m")
	session, err := NewSession(rt, WithDigest("sha256:override"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = session.resumeDigest()
	}
}
