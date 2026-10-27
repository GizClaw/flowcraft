package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/projection/vector"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// TestVectorLaneStatsWireTheLane pins the diagnostics wiring end to end: a
// deployment built without an embedding model reports no vector lane, and one
// built with it hands out readings that follow what the lane served.
func TestVectorLaneStatsWireTheLane(t *testing.T) {
	withoutEmbed, _ := newTestAssembly(t, "")
	if _, ok := withoutEmbed.VectorLaneStats(); ok {
		t.Fatal("an assembly without an embedding model reported vector lane stats")
	}

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
	engine := fake.Assembly(t)
	settings := fmt.Sprintf(`{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "embed": {"provider": %q, "name": %q, "profile": %q},
	  "interval": "0"
	}`, inferencetest.DefaultFakeEmbedModel.ID.Provider, inferencetest.DefaultFakeEmbedModel.ID.Name,
		inferencetest.DefaultFakeEmbedModel.Profile)
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	value, err := NewFactory(
		WithClock(func() time.Time { return testNow }),
		WithDeriver(fakeDeriver{}),
	).New(context.Background(), resource.NewInput([]byte(settings), map[string]any{"workspace": ws, "inference": engine}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	assembly := value
	if err := assembly.Wire(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, ok := assembly.VectorLaneStats()
	if !ok {
		t.Fatal("an assembly with an embedding model reported no vector lane stats")
	}
	if before.Queries != 0 || before.Footprint != (vector.Footprint{}) {
		t.Fatalf("stats before any request = %+v", before)
	}

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
	if _, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "tea",
	}); err != nil {
		t.Fatal(err)
	}
	after, ok := assembly.VectorLaneStats()
	if !ok {
		t.Fatal("vector lane stats disappeared")
	}
	if after.Queries < 1 || after.Scans < 1 || after.Embeds < 1 {
		t.Fatalf("queries/scans/embeds = %d/%d/%d", after.Queries, after.Scans, after.Embeds)
	}
	if after.Footprint.Shards < 1 || after.Footprint.Entries < 1 || after.Footprint.VectorBytes <= 0 {
		t.Fatalf("footprint = %+v", after.Footprint)
	}
}
