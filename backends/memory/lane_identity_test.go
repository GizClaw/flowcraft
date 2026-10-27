package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	projectionstore "github.com/GizClaw/flowcraft/backends/memory/internal/projection"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// staleBM25ProducerVersion is a producer version no lane writes today: state
// stamped with it is what an earlier splitter left behind.
const staleBM25ProducerVersion = "okapi-bm25-v1"

// legacyBM25Snapshot and legacyBM25Document mirror the bm25 lane's persisted
// build without reaching into its private types, because that is the shape a
// test has to write to seed state an earlier producer left: a projection
// survives a producer change precisely because the storage format does.
type legacyBM25Snapshot struct {
	K1            float64              `json:"k1"`
	B             float64              `json:"b"`
	AverageLength float64              `json:"average_length"`
	Documents     []legacyBM25Document `json:"documents"`
}

type legacyBM25Document struct {
	ID            string                     `json:"id"`
	Name          string                     `json:"name"`
	Length        int                        `json:"length"`
	Terms         map[string]int             `json:"terms"`
	Source        corememory.SourceRef       `json:"source"`
	Address       component.CandidateAddress `json:"address"`
	Metadata      corememory.Metadata        `json:"metadata,omitempty"`
	ContentDigest string                     `json:"content_digest"`
}

// factLaneEntryID reproduces the address a fact carries inside the projection
// lanes (worker.factLaneID): the fact id is a content address, so the lane
// qualifies it with the conversation that derived it.
func factLaneEntryID(conversationID, factID string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"fact", conversationID, factID}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// seedStaleBM25Lane replaces one scope's bm25 lane with the state an earlier
// producer left: a manifest stamped with that producer's version and one build
// whose terms are that producer's. The document's terms here are the whole run
// "你好世界" as a single term -- what a splitter without CJK support wrote, and
// what the characters and bigrams a query produces now cannot reach.
func seedStaleBM25Lane(
	t *testing.T,
	ws workspace.Workspace,
	scope corememory.Scope,
	projection, conversationID, factID, text string,
) {
	t.Helper()
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := projectionstore.NewTypedStore[legacyBM25Snapshot, projectionstore.EntryDelta[legacyBM25Document]](
		kvStore, "bm25",
		projectionstore.TypedOptions[legacyBM25Snapshot, projectionstore.EntryDelta[legacyBM25Document]]{
			ProducerVersion: staleBM25ProducerVersion,
			Apply: func(base *legacyBM25Snapshot, delta projectionstore.EntryDelta[legacyBM25Document]) error {
				base.Documents = append(base.Documents, delta.Upserts...)
				return nil
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	terms := map[string]int{text: 1}
	snapshot := legacyBM25Snapshot{
		K1: 1.2, B: 0.75, AverageLength: 1,
		Documents: []legacyBM25Document{{
			ID: factLaneEntryID(conversationID, factID), Name: string(corememory.ContextFact), Length: 1, Terms: terms,
			Source: corememory.SourceRef{Kind: corememory.SourceMessage, ID: conversationID + "/message-1", Revision: "1"},
			Address: component.CandidateAddress{
				Kind: corememory.ContextFact, ConversationID: conversationID, ItemID: factID,
			},
			Metadata: corememory.Metadata{
				"context_kind": string(corememory.ContextFact), "item_id": factID, "conversation_id": conversationID,
				"runtime_id": scope.RuntimeID, "user_id": scope.UserID,
			},
			ContentDigest: strings.Repeat("0", 64),
		}},
	}
	shard, err := component.ConversationShard(conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FullRebuild(ctx, projectionstore.Address{Scope: scope, Shard: shard},
		projection, snapshot, "legacy-source-digest"); err != nil {
		t.Fatal(err)
	}
}

func laneNamed(t *testing.T, lanes []LaneDiagnostics, name string) LaneDiagnostics {
	t.Helper()
	for _, lane := range lanes {
		if lane.Name == name {
			return lane
		}
	}
	t.Fatalf("lane %q missing from %+v", name, lanes)
	return LaneDiagnostics{}
}

// derivedLaneAssembly derives one fact for one conversation, returns the
// assembly, its workspace, and the fact's id.
func derivedLaneAssembly(t *testing.T) (*Assembly, workspace.Workspace, string) {
	t.Helper()
	assembly, ws := newTestAssemblyWith(t, testSettings, WithDeriver(perInputDeriver{}))
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "你好世界")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	facts, err := assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %d, want 1", len(facts))
	}
	return assembly, ws, facts[0].ID
}

// A lane reports the identity of the state it reads, and nothing else: a lane
// that holds no state is not stale, and a lane whose state an earlier producer
// wrote reports the version it was stamped with instead of the one the running
// lane writes with.
func TestLaneStatusReportsTheStoredProducer(t *testing.T) {
	ctx := context.Background()
	assembly, ws, factID := derivedLaneAssembly(t)
	lanes, err := assembly.LaneStatus(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	for _, lane := range lanes {
		if !lane.Stored || lane.Stale || lane.StoredProducer != lane.ExpectedProducer || lane.ExpectedProducer == "" {
			t.Fatalf("lane derived by the running producer reported %+v", lane)
		}
	}
	if lane := laneNamed(t, lanes, "bm25"); lane.ExpectedProducer != "okapi-bm25-v2+k1=1.2,b=0.75" {
		t.Fatalf("bm25 expected producer = %q", lane.ExpectedProducer)
	}

	seedStaleBM25Lane(t, ws, testScope(), "facts", "conv-1", factID, "你好世界")
	lanes, err = assembly.LaneStatus(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	bm25 := laneNamed(t, lanes, "bm25")
	if !bm25.Stored || !bm25.Stale || bm25.StoredProducer != staleBM25ProducerVersion {
		t.Fatalf("seeded lane reported %+v", bm25)
	}
	if entity := laneNamed(t, lanes, "entity"); entity.Stale {
		t.Fatalf("entity lane reported stale = %+v", entity)
	}

	// The same condition reaches the read-only plan the host checks with.
	plan, err := assembly.Verify(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := false
	for _, action := range plan.Actions {
		if action.Kind == "rebuild" && action.Target == "projection:bm25" &&
			strings.Contains(action.Evidence, "producer version mismatch: stored "+staleBM25ProducerVersion) {
			rebuilt = true
		}
	}
	if !rebuilt {
		t.Fatalf("verify plan did not report the stale lane: %+v", plan.Actions)
	}

	// And the assembly-wide snapshot carries it per scope, which is where a
	// host that does not know which scope to ask looks.
	diagnostics, err := assembly.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics.Scopes) != 1 {
		t.Fatalf("diagnostics scopes = %+v", diagnostics.Scopes)
	}
	if stale := laneNamed(t, diagnostics.Scopes[0].Lanes, "bm25"); !stale.Stale {
		t.Fatalf("assembly diagnostics reported %+v", stale)
	}
}
