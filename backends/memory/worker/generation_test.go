package worker

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	summaryderive "github.com/GizClaw/flowcraft/backends/memory/derive/summary"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// policyDeriver derives one fact per commit whose text names the policy that
// derived it, so a re-derivation under a new policy is distinguishable from the
// generation it replaces. Its fact ids are the content address of the fact text
// alone, exactly like the chat line's, so two conversations that derive the
// same text derive the same id.
type policyDeriver struct{ prefix string }

func (deriver policyDeriver) Derive(_ context.Context, source component.Artifact) ([]component.Artifact, error) {
	text := strings.TrimSpace(source.Content.Text())
	if text == "" {
		return nil, nil
	}
	body := deriver.prefix + ": " + text
	metadata := corememory.Metadata{}
	for key, value := range source.Metadata {
		metadata[key] = value
	}
	metadata["canonical_hash"] = factview.CanonicalHash(body)
	metadata["source_digest"] = factview.ComputeSourceDigest(source.Sources)
	metadata["transform_signature"] = "policy-derive-v1"
	return []component.Artifact{{
		Kind: chat.KindFact, ID: contentFactID(body),
		Content:  coremessage.NewTextContent(body),
		Sources:  append([]corememory.SourceRef(nil), source.Sources...),
		Metadata: metadata,
	}}, nil
}

func contentFactID(text string) string {
	_, hash, _ := strings.Cut(factview.CanonicalHash(text), ":")
	return "fact-" + hash
}

// recordingLane keeps the entries one lane holds, keyed and addressed the way
// the real lanes key them, so a test can see what a converge leaves behind.
type recordingLane struct {
	mu      sync.Mutex
	entries map[string]component.CandidateAddress
	deltas  []component.ProjectionDelta
	// failNextConverge fails the next delta that publishes a generation,
	// which is how a converge failure is staged.
	failNextConverge error
}

func newRecordingLane() *recordingLane {
	return &recordingLane{entries: map[string]component.CandidateAddress{}}
}

func (lane *recordingLane) ApplyDelta(_ context.Context, delta component.ProjectionDelta) error {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.failNextConverge != nil && strings.HasPrefix(delta.SourceRevision, "generation:") {
		err := lane.failNextConverge
		lane.failNextConverge = nil
		return err
	}
	for _, id := range delta.DeleteIDs {
		delete(lane.entries, id)
	}
	for _, artifact := range delta.Upserts {
		lane.entries[artifact.ID] = component.AddressFromArtifact(artifact)
	}
	lane.deltas = append(lane.deltas, delta)
	return nil
}

// projected returns the ids of the fact entries addressed to one conversation,
// sorted. Message entries are filtered out: they are canonical rather than
// derived, so a generation switch neither adds nor removes them.
func (lane *recordingLane) projected(conversationID string) []string {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	ids := make([]string, 0, len(lane.entries))
	for id, address := range lane.entries {
		if address.Kind == corememory.ContextFact && address.ConversationID == conversationID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// messages returns the ids of the message entries addressed to one
// conversation, sorted.
func (lane *recordingLane) messages(conversationID string) []string {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	ids := make([]string, 0, len(lane.entries))
	for id, address := range lane.entries {
		if address.Kind == corememory.ContextRawMessage && address.ConversationID == conversationID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (lane *recordingLane) factCount() int {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	count := 0
	for _, address := range lane.entries {
		if address.Kind == corememory.ContextFact {
			count++
		}
	}
	return count
}

func (lane *recordingLane) deltaCount() int {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return len(lane.deltas)
}

// lastConverge returns the generation-publishing delta the lane applied last.
func (lane *recordingLane) lastConverge(t *testing.T) component.ProjectionDelta {
	t.Helper()
	lane.mu.Lock()
	defer lane.mu.Unlock()
	for index := len(lane.deltas) - 1; index >= 0; index-- {
		if strings.HasPrefix(lane.deltas[index].SourceRevision, "generation:") {
			return lane.deltas[index]
		}
	}
	t.Fatal("no generation converge reached the lane")
	return component.ProjectionDelta{}
}

// generationStore is the derived state two processors of one workspace share: a
// policy change builds a new assembly over the same store.
type generationStore struct {
	facts     *factview.FactStore
	summaries *summaryview.SummaryStore
	compactor *summaryderive.Compactor
}

func newGenerationStore(t *testing.T) *generationStore {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store := &generationStore{}
	store.facts, err = factview.NewFactStore(logStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	store.summaries, err = summaryview.NewSummaryStore(logStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	store.compactor, err = summaryderive.New(
		summaryderive.DefaultConfig(), store.summaries, summaryderive.ExtractiveSummarizer{})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newGenerationProcessor(
	t *testing.T,
	messages MessageReader,
	checkpoints CheckpointStore,
	store *generationStore,
	policyDigest string,
	deriver component.Deriver,
	lane *recordingLane,
) *Processor {
	t.Helper()
	processor, err := NewProcessor(Config{
		Messages: messages, Facts: store.facts, Deriver: deriver, Checkpoints: checkpoints,
		Compactor: store.compactor, Projection: "test", PolicyDigest: policyDigest,
		Indexers: []ProjectionIndexer{{Name: "test", Indexer: lane}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func conversationCommit(conversationID, text string) msgsource.Commit {
	return msgsource.Commit{
		ID: "commit-" + conversationID, Scope: corememory.Scope{RuntimeID: "memories"}, Version: 1,
		ConversationID: conversationID,
		Records: []msgsource.Record{{
			ID: "msg-" + conversationID, ConversationID: conversationID, Seq: 1,
			Message: coremessage.NewTextMessage(coremessage.RoleUser, text),
		}},
	}
}

// TestGenerationSwitchConvergesTheProjectionLanes pins the converge contract:
// a switch leaves the lanes holding the published generation and nothing of the
// generation it replaced, a rollback brings the facts an earlier switch pruned
// back, and a pass that publishes the generation already visible writes no
// converge at all -- so a switched workspace does not re-project on every pass.
func TestGenerationSwitchConvergesTheProjectionLanes(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: {conversationCommit(conversationID, "we talked about beverages")},
	}}
	checkpoints := newMemoryCheckpoints()
	lane := newRecordingLane()
	store := newGenerationStore(t)

	first := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	if _, err := first.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	derived := lane.projected(conversationID)
	if len(derived) != 1 {
		t.Fatalf("projected entries = %#v, want one", derived)
	}
	messagesBefore := lane.messages(conversationID)
	if len(messagesBefore) != 1 {
		t.Fatalf("projected messages = %#v, want one", messagesBefore)
	}
	// Adopting the first generation has nothing to converge: no switch ever
	// pruned this conversation, so the lanes only saw the derive delta.
	if convergences := first.Stats().GenerationConvergences; convergences != 0 {
		t.Fatalf("convergences = %d, want 0", convergences)
	}

	second := newGenerationProcessor(t, messages, checkpoints, store, "policy-b",
		policyDeriver{prefix: "b"}, lane)
	if _, err := second.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	if active, found, err := store.facts.ActiveGeneration(ctx, scope, conversationID); err != nil || !found || active != "policy-b" {
		t.Fatalf("active generation = %q, %v, %v", active, found, err)
	}
	switched := lane.projected(conversationID)
	if len(switched) != 1 || switched[0] == derived[0] {
		t.Fatalf("projected entries after the switch = %#v, want one entry replacing %#v", switched, derived)
	}
	converge := lane.lastConverge(t)
	if !contains(converge.DeleteIDs, derived[0]) {
		t.Fatalf("converge deletes = %#v, want the replaced entry %q", converge.DeleteIDs, derived[0])
	}
	if !contains(artifactIDs(converge.Upserts), switched[0]) {
		t.Fatalf("converge upserts = %#v, want the active entry %q", artifactIDs(converge.Upserts), switched[0])
	}
	if convergences := second.Stats().GenerationConvergences; convergences != 1 {
		t.Fatalf("convergences = %d, want 1", convergences)
	}
	if messagesAfter := lane.messages(conversationID); len(messagesAfter) != 1 || messagesAfter[0] != messagesBefore[0] {
		t.Fatalf("the switch changed the projected messages: %#v, want %#v", messagesAfter, messagesBefore)
	}

	// The replaced generation is still stored, which is what makes a rollback
	// possible: switching back re-projects the facts the outbound switch
	// pruned instead of serving them half-present.
	third := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	if _, err := third.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	rolledBack := lane.projected(conversationID)
	if len(rolledBack) != 1 || rolledBack[0] != derived[0] {
		t.Fatalf("projected entries after the rollback = %#v, want %#v", rolledBack, derived)
	}
	if active, found, err := store.facts.ActiveGeneration(ctx, scope, conversationID); err != nil || !found || active != "policy-a" {
		t.Fatalf("active generation = %q, %v, %v", active, found, err)
	}

	// Steady state: the generation is already published and nothing is
	// replaced, so another pass must not re-project anything.
	deltas, convergences := lane.deltaCount(), third.Stats().GenerationConvergences
	if _, err := third.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	if lane.deltaCount() != deltas || third.Stats().GenerationConvergences != convergences {
		t.Fatalf("a steady-state pass wrote deltas: %d -> %d, convergences %d -> %d",
			deltas, lane.deltaCount(), convergences, third.Stats().GenerationConvergences)
	}
}

// TestConvergenceKeepsAnotherConversationsFact pins the lane identity: facts are
// content-addressed, so two conversations deriving the same text derive the same
// fact id, and a lane is partitioned by scope rather than by conversation. The
// entries stay separate, and converging one conversation never removes the
// other's fact.
func TestConvergenceKeepsAnotherConversationsFact(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const sharedText = "my favourite drink is tea"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		"conv-a": {conversationCommit("conv-a", sharedText)},
		"conv-b": {conversationCommit("conv-b", sharedText)},
	}}
	checkpoints := newMemoryCheckpoints()
	lane := newRecordingLane()
	store := newGenerationStore(t)

	first := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	for _, conversationID := range []string{"conv-a", "conv-b"} {
		if _, err := first.ProcessConversation(ctx, scope, conversationID); err != nil {
			t.Fatal(err)
		}
	}
	firstA, firstB := lane.projected("conv-a"), lane.projected("conv-b")
	if len(firstA) != 1 || len(firstB) != 1 {
		t.Fatalf("projected entries = %#v / %#v, want one each", firstA, firstB)
	}
	if firstA[0] == firstB[0] {
		t.Fatalf("two conversations share the projection entry %q", firstA[0])
	}
	if entries := lane.factCount(); entries != 2 {
		t.Fatalf("lane holds %d entries, want 2", entries)
	}

	second := newGenerationProcessor(t, messages, checkpoints, store, "policy-b",
		policyDeriver{prefix: "b"}, lane)
	if _, err := second.ProcessConversation(ctx, scope, "conv-a"); err != nil {
		t.Fatal(err)
	}
	secondB := lane.projected("conv-b")
	if len(secondB) != 1 || secondB[0] != firstB[0] {
		t.Fatalf("converging conv-a dropped conv-b's fact: %#v, want %#v", secondB, firstB)
	}
	if switched := lane.projected("conv-a"); len(switched) != 1 || switched[0] == firstA[0] {
		t.Fatalf("projected entries of conv-a = %#v, want one entry replacing %#v", switched, firstA)
	}
	if entries := lane.factCount(); entries != 2 {
		t.Fatalf("lane holds %d entries, want 2", entries)
	}
}

// TestConvergeFailureKeepsThePublishedGeneration checks the switch is atomic in
// the direction that matters: the lanes are reconciled before the generation is
// published, so a lane that refuses the converge leaves the previous generation
// readable, and the next pass retries the switch.
func TestConvergeFailureKeepsThePublishedGeneration(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: {conversationCommit(conversationID, "we talked about beverages")},
	}}
	checkpoints := newMemoryCheckpoints()
	lane := newRecordingLane()
	store := newGenerationStore(t)

	first := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	if _, err := first.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	derived := lane.projected(conversationID)

	second := newGenerationProcessor(t, messages, checkpoints, store, "policy-b",
		policyDeriver{prefix: "b"}, lane)
	lane.failNextConverge = errors.New("lane is down")
	if _, err := second.ProcessConversation(ctx, scope, conversationID); err == nil {
		t.Fatal("a failed converge was reported as a successful pass")
	} else if !strings.Contains(err.Error(), "lane is down") {
		t.Fatalf("failure does not name the lane: %v", err)
	}
	if active, found, err := store.facts.ActiveGeneration(ctx, scope, conversationID); err != nil || !found || active != "policy-a" {
		t.Fatalf("active generation = %q, %v, %v, want the generation that never switched", active, found, err)
	}
	if kept := lane.projected(conversationID); !contains(kept, derived[0]) {
		t.Fatalf("the failed converge pruned the published generation's entry: %#v", kept)
	}

	if _, err := second.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	if active, found, err := store.facts.ActiveGeneration(ctx, scope, conversationID); err != nil || !found || active != "policy-b" {
		t.Fatalf("active generation after the retry = %q, %v, %v", active, found, err)
	}
	if convergences := second.Stats().GenerationConvergences; convergences != 1 {
		t.Fatalf("convergences = %d, want 1", convergences)
	}
}

// TestGenerationSwitchRepublishesTheSummaryBranch pins what a pass that derives
// nothing still has to do: a rollback finds the generation's watermark already
// at the end of the stream, so no commit is compacted, and the summaries the
// branch serves have to move back with the same switch that moves the facts.
func TestGenerationSwitchRepublishesTheSummaryBranch(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: {conversationCommit(conversationID, "we talked about beverages")},
	}}
	checkpoints := newMemoryCheckpoints()
	lane := newRecordingLane()
	store := newGenerationStore(t)

	first := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	if _, err := first.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	firstManifest := activeSummaryManifest(t, store, scope, conversationID)
	if firstManifest.GenerationID != "policy-a" {
		t.Fatalf("summary generation = %q, want policy-a", firstManifest.GenerationID)
	}

	second := newGenerationProcessor(t, messages, checkpoints, store, "policy-b",
		policyDeriver{prefix: "b"}, lane)
	if _, err := second.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	if switched := activeSummaryManifest(t, store, scope, conversationID); switched.GenerationID != "policy-b" {
		t.Fatalf("summary generation after the switch = %q, want policy-b", switched.GenerationID)
	}

	third := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	processed, err := third.ProcessConversation(ctx, scope, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 0 {
		t.Fatalf("the rollback derived %d commits, want none", processed)
	}
	if restored := activeSummaryManifest(t, store, scope, conversationID); !reflect.DeepEqual(restored, firstManifest) {
		t.Fatalf("restored summaries = %#v, want %#v", restored, firstManifest)
	}
	// The branch serves the rolled-back generation's own leaves, summarising
	// the facts that generation derived.
	generationFacts, err := store.facts.List(ctx, scope, conversationID, factview.ListOptions{Generation: "policy-a"})
	if err != nil {
		t.Fatal(err)
	}
	live := make(map[string]struct{}, len(generationFacts))
	for _, fact := range generationFacts {
		live[fact.ID] = struct{}{}
	}
	records, err := store.summaries.ListActive(ctx, scope, conversationID,
		summaryview.ListOptions{GenerationID: "policy-a"})
	if err != nil || len(records) == 0 {
		t.Fatalf("summaries of the rolled-back generation = %#v, %v", records, err)
	}
	for _, record := range records {
		if record.GenerationID != "policy-a" {
			t.Fatalf("summary %q belongs to generation %q", record.ID, record.GenerationID)
		}
		if record.Level != summaryview.L0 {
			continue
		}
		for _, inputID := range record.InputIDs {
			if _, ok := live[inputID]; !ok {
				t.Fatalf("summary %q summarises fact %q of another generation", record.ID, inputID)
			}
		}
	}
}

// activeSummaryManifest returns the manifest the summary branch serves.
func activeSummaryManifest(
	t *testing.T,
	store *generationStore,
	scope corememory.Scope,
	conversationID string,
) summaryview.Manifest {
	t.Helper()
	manifest, found, err := store.summaries.LoadActive(context.Background(), scope, conversationID)
	if err != nil || !found {
		t.Fatalf("active summary manifest = %v, %v", found, err)
	}
	return manifest
}

func artifactIDs(artifacts []component.Artifact) []string {
	ids := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		ids = append(ids, artifact.ID)
	}
	return ids
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
