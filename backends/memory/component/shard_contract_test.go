package component

import (
	"strings"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func deltaArtifact(id, conversationID string) Artifact {
	return Artifact{
		ID: id,
		Metadata: corememory.Metadata{
			"context_kind": string(corememory.ContextFact), "conversation_id": conversationID,
		},
	}
}

// TestProjectionDeltaNamesOneShard pins the write-side address contract: a
// delta states the shard it lands in, everything it carries belongs to that
// shard, and a delta that spans two addresses is refused -- the store routes
// deletes by the delta's shard, so a mixed one would delete from a shard it
// never named.
func TestProjectionDeltaNamesOneShard(t *testing.T) {
	conversation, _ := ConversationShard("c1")
	other, _ := ConversationShard("c2")
	dataset, _ := DatasetShard("d1")

	valid := ProjectionDelta{Shard: conversation, Upserts: []Artifact{
		deltaArtifact("one", "c1"), deltaArtifact("two", "c1"),
	}}
	if err := valid.ValidateShard(); err != nil {
		t.Fatalf("valid delta: %v", err)
	}
	// A delete-only delta (a retired generation) has no upsert to infer an
	// address from, which is why the shard is a field of the delta.
	if err := (ProjectionDelta{Shard: conversation, DeleteIDs: []string{"one"}}).ValidateShard(); err != nil {
		t.Fatalf("delete-only delta: %v", err)
	}
	for name, delta := range map[string]ProjectionDelta{
		"no shard":        {Upserts: []Artifact{deltaArtifact("one", "c1")}},
		"other shard":     {Shard: other, Upserts: []Artifact{deltaArtifact("one", "c1")}},
		"mixed upserts":   {Shard: conversation, Upserts: []Artifact{deltaArtifact("one", "c1"), deltaArtifact("two", "c2")}},
		"document delete": {Shard: conversation, DeleteDocuments: []DocumentAddress{{DatasetID: "d1", DocumentID: "doc"}}},
		"document keeps":  {Shard: conversation, ReconcileDocuments: []DocumentAddress{{DatasetID: "d1", DocumentID: "doc"}}},
		"document in place": {
			Shard: dataset, ReconcileDocuments: []DocumentAddress{{DatasetID: "d1", DocumentID: "doc"}},
		},
	} {
		err := delta.ValidateShard()
		if name == "document in place" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%s: empty error", name)
		}
	}
	// An artifact whose address cannot be computed has no shard to land in.
	unaddressed := ProjectionDelta{Shard: conversation, Upserts: []Artifact{{ID: "orphan"}}}
	if err := unaddressed.ValidateShard(); err == nil {
		t.Error("an artifact without an address was accepted")
	}
}
