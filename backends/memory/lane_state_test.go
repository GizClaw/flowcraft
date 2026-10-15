package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// chainText concatenates the messages of an error chain. The memory error
// renders only its kind and capability, so the lane's own report is what the
// chain beneath it holds.
func chainText(err error) string {
	var text strings.Builder
	for current := err; current != nil; current = errors.Unwrap(current) {
		text.WriteString(current.Error())
		text.WriteString(": ")
	}
	return text.String()
}

// vectorAssemblySettings configures the fixture's deployment: a fake embedding
// model, because the vector lane is only built when one exists, and no summary
// branch, so the state the test tampers with is the projection lanes' alone.
func vectorAssemblySettings() string {
	return fmt.Sprintf(`{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "embed": {"provider": %q, "name": %q, "profile": %q},
	  "interval": "0"
	}`, inferencetest.DefaultFakeEmbedModel.ID.Provider, inferencetest.DefaultFakeEmbedModel.ID.Name,
		inferencetest.DefaultFakeEmbedModel.Profile)
}

// newVectorAssemblyOn builds the fixture's assembly over one workspace. Every
// call reads the workspace afresh: the materialized tables of the assembly
// that wrote the state are not shared, so a later call's first query is the
// first read of a process.
func newVectorAssemblyOn(t *testing.T, ws workspace.Workspace) *Assembly {
	t.Helper()
	fake := &inferencetest.EmbedFake{Respond: func(request inference.EmbedRequest) inference.EmbedResponse {
		embeddings := make([]inference.Embedding, len(request.Items))
		for index, item := range request.Items {
			if strings.Contains(strings.ToLower(item.Content.Text()), "tea") {
				embeddings[index] = inference.Embedding{Vector: []float32{1, 0}}
			} else {
				embeddings[index] = inference.Embedding{Vector: []float32{0, 1}}
			}
		}
		return inference.EmbedResponse{Embeddings: embeddings}
	}}
	value, err := NewFactory(
		WithClock(func() time.Time { return testNow }),
		WithDeriver(fakeDeriver{}),
	).New(context.Background(), resource.Input{
		Settings: []byte(vectorAssemblySettings()),
		Deps:     map[string]any{"workspace": ws, "inference": fake.Assembly(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	assembly, ok := value.(*Assembly)
	if !ok {
		t.Fatalf("factory returned %T, want *Assembly", value)
	}
	if err := assembly.Wire(context.Background()); err != nil {
		t.Fatal(err)
	}
	return assembly
}

// derivedVectorWorkspace derives one fact over a workspace with an embedding
// model, so the vector lane holds state to read.
func derivedVectorWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	assembly := newVectorAssemblyOn(t, ws)
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "Alice likes tea")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	return ws
}

// corruptVectorLane rewrites one stored payload of the vector lane with a
// flipped byte: the manifest stays as it was, so the state exists and no read
// of it can pass the digest check the manifest carries. That is the condition
// a reader reports as a mismatch until Repair rewrites the lane.
func corruptVectorLane(t *testing.T, ctx context.Context, ws workspace.Workspace) {
	t.Helper()
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := kv.List(ctx, "projections/vector")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Key, ".json") || strings.HasSuffix(entry.Key, "active.json") {
			continue
		}
		payload := bytes.Clone(entry.Value)
		if len(payload) == 0 {
			continue
		}
		payload[len(payload)/2] ^= 0x01
		if err := kv.Put(ctx, entry.Key, payload); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("the vector lane stored no payload under its keys: %+v", entries)
}

// A lane whose stored state cannot be materialized did not answer, and that is
// not the same as a lane that found nothing: the remaining lanes answer, so
// the recall the unreadable lane held is missing from a result that says
// nothing about it. The request fails instead, and the lane counts the read it
// could not serve apart from the reads that found a table, which is what makes
// the difference observable. A scope that holds no state at all still answers:
// a lane with nothing to read is not a lane that failed.
func TestUnreadableLaneStateFailsTheRequest(t *testing.T) {
	ctx := context.Background()
	ws := derivedVectorWorkspace(t)
	// The control: an assembly built after the writes reads the state afresh
	// and answers from it. Its table cache is its own, so the query that
	// follows in the next assembly cannot be served from what this one read.
	control := newVectorAssemblyOn(t, ws)
	if !answeredLongTerm(t, control, "Alice likes tea") {
		t.Fatal("the fixture does not answer from its derived state")
	}
	corruptVectorLane(t, ctx, ws)
	reopened := newVectorAssemblyOn(t, ws)
	_, err := reopened.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "Alice likes tea",
	})
	if err == nil {
		t.Fatal("a lane that cannot read its state answered as if it found nothing")
	}
	if !corememory.IsKind(err, corememory.KindProviderFailure) {
		t.Fatalf("the failure is not classified as a provider failure: %v", err)
	}
	var state *component.StateError
	if !errors.As(err, &state) {
		t.Fatalf("the failure does not carry the unreadable state: %v", err)
	}
	if !strings.Contains(state.Projection, "vector") {
		t.Fatalf("the failure names the wrong state: %q", state.Projection)
	}
	if !strings.Contains(chainText(err), `lane "vector"`) {
		t.Fatalf("the failure does not name the lane it came from: %s", chainText(err))
	}
	stats, ok := reopened.VectorLaneStats()
	if !ok {
		t.Fatal("the fixture builds no vector lane")
	}
	if stats.SnapshotErrors != 1 || stats.SnapshotHits != 0 || stats.SnapshotMisses != 0 {
		t.Fatalf("the failed read was not counted apart from hits and loads: %+v", stats)
	}
}

// A scope that was never derived holds no lane state at all, and that is not a
// failure: the lanes have nothing to read, and the request is still answered
// from what the scope does hold.
func TestScopeWithNoLaneStateStillAnswers(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	assembly := newVectorAssemblyOn(t, ws)
	result, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "Alice likes tea",
	})
	if err != nil {
		t.Fatalf("a scope with no lane state failed its request: %v", err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("items = %+v", result.Items)
	}
}
