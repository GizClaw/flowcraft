package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// streamCommits builds the commits of one conversation with distinct ids and
// increasing versions, the way the message store numbers them.
func streamCommits(conversationID string, texts ...string) []msgsource.Commit {
	commits := make([]msgsource.Commit, 0, len(texts))
	for index, text := range texts {
		version := uint64(index + 1)
		commit := conversationCommit(conversationID, text)
		commit.ID = fmt.Sprintf("commit-%s-%d", conversationID, version)
		commit.Version = version
		commit.Records[0].ID = fmt.Sprintf("msg-%s-%d", conversationID, version)
		commit.Records[0].Seq = version
		commits = append(commits, commit)
	}
	return commits
}

// flakyDeriver derives like a policy deriver until it has succeeded the number
// of times the test allows, then fails: a pass that stops there has stored and
// projected what the commits before the failure derived -- and advanced its
// watermark over them -- but never published its generation.
type flakyDeriver struct {
	deriver policyDeriver
	allows  int
	calls   int
}

func (deriver *flakyDeriver) Derive(ctx context.Context, source component.Artifact) ([]component.Artifact, error) {
	deriver.calls++
	if deriver.calls > deriver.allows {
		return nil, errors.New("memory: derivation is unavailable")
	}
	return deriver.deriver.Derive(ctx, source)
}

// TestRetireProgressRefusesThePublishedGeneration pins the guard on the cursor
// half of retirement: reads resolve the published generation, so its progress is
// what keeps an ordinary pass from re-deriving a generation that is already
// visible -- and a re-derivation is the model's, so the visible generation would
// grow beside itself instead of converging.
func TestRetireProgressRefusesThePublishedGeneration(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: streamCommits(conversationID, "we talked about beverages", "and about tea"),
	}}
	checkpoints := newMemoryCheckpoints()
	store := newGenerationStore(t)
	processor := newGenerationProcessor(t, messages, checkpoints, store, "policy-a", policyDeriver{prefix: "a"}, newRecordingLane())
	if _, err := processor.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	before := checkpoints.snapshot()

	if _, err := processor.RetireProgress(ctx, scope, conversationID, []string{"policy-a"}); err == nil {
		t.Fatal("the published generation's progress was retired")
	}
	if after := checkpoints.snapshot(); len(after) != len(before) {
		t.Fatalf("cursors after the refusal = %#v, want %#v", after, before)
	}
	// A generation this conversation never derived under is not progress to
	// retire, and a sweep of one is not an error: it finds nothing to drop.
	if retired, err := processor.RetireProgress(ctx, scope, conversationID, []string{"policy-b"}); err != nil || retired != 0 {
		t.Fatalf("retired = %d, %v, want 0", retired, err)
	}
}

// TestRetireProgressLetsARollbackDeriveTheWholeStream is the reason the cursor
// belongs to the generation it derives. A pass that stopped before publishing
// left its watermark mid-stream over facts that a sweep is about to retire; a
// rollback that resumed at that cursor would derive only the commits after it
// and publish a generation missing the rest. Retiring the progress with the
// generation makes the rollback derive the stream from its head instead.
func TestRetireProgressLetsARollbackDeriveTheWholeStream(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: streamCommits(conversationID, "we talked about beverages", "and about tea"),
	}}
	checkpoints := newMemoryCheckpoints()
	lane := newRecordingLane()
	store := newGenerationStore(t)

	// The conversation is already served by another generation, which is what
	// the failing pass leaves in place: a conversation with nothing published
	// adopts the first generation written, and an adopted generation is one
	// reads resolve.
	published := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	if _, err := published.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	abandoned := newGenerationProcessor(t, messages, checkpoints, store, "policy-b",
		&flakyDeriver{deriver: policyDeriver{prefix: "b"}, allows: 1}, lane)
	if _, err := abandoned.ProcessConversation(ctx, scope, conversationID); err == nil {
		t.Fatal("the failing pass reported success")
	}
	if active, found, err := store.facts.ActiveGeneration(ctx, scope, conversationID); err != nil || !found || active != "policy-a" {
		t.Fatalf("active generation after the failing pass = %q, %v, %v", active, found, err)
	}
	stored, err := store.facts.List(ctx, scope, conversationID, factview.ListOptions{Generation: "policy-b"})
	if err != nil || len(stored) != 1 {
		t.Fatalf("facts of the abandoned generation = %#v, %v, want the first commit's", stored, err)
	}
	if cursor := checkpoints.snapshot()[checkpointKey(scope, streamKindMessages, conversationID, "policy-b")]; cursor != 1 {
		t.Fatalf("watermark of the abandoned generation = %d, want 1", cursor)
	}

	if retired, err := abandoned.RetireProgress(ctx, scope, conversationID, []string{"policy-b"}); err != nil || retired != 1 {
		t.Fatalf("retired = %d, %v, want the abandoned generation's cursor", retired, err)
	}

	// The rollback derives the stream from its head, not from the cursor the
	// abandoned pass left behind.
	rollback := newGenerationProcessor(t, messages, checkpoints, store, "policy-b",
		policyDeriver{prefix: "b"}, lane)
	processed, err := rollback.ProcessConversation(ctx, scope, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 2 {
		t.Fatalf("the rollback derived %d commits, want the whole stream", processed)
	}
	derived, err := store.facts.List(ctx, scope, conversationID, factview.ListOptions{Generation: "policy-b"})
	if err != nil || len(derived) != 2 {
		t.Fatalf("facts of the rolled-back generation = %#v, %v, want both commits", derived, err)
	}
	if active, found, err := store.facts.ActiveGeneration(ctx, scope, conversationID); err != nil || !found || active != "policy-b" {
		t.Fatalf("active generation = %q, %v, %v", active, found, err)
	}
}
