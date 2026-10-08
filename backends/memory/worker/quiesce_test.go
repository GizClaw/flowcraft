package worker

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// blockingDeriver derives like the ordinary test policy until the test arms it,
// then holds every call until the test releases it: a pass that reaches it stays
// in flight for as long as the test needs it to.
type blockingDeriver struct {
	inner   component.Deriver
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (deriver *blockingDeriver) Derive(ctx context.Context, source component.Artifact) ([]component.Artifact, error) {
	if deriver.armed.Load() {
		deriver.entered <- struct{}{}
		<-deriver.release
	}
	return deriver.inner.Derive(ctx, source)
}

// TestDerivationStateReportsAPassInFlight pins the first half of the sweep
// guard: a pass over the conversation holds the conversation unsettled until it
// returns, and the refusal names the pass rather than the conversation.
func TestDerivationStateReportsAPassInFlight(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: {conversationCommit(conversationID, "we talked about beverages")},
	}}
	deriver := &blockingDeriver{
		inner: policyDeriver{prefix: "a"}, entered: make(chan struct{}, 4), release: make(chan struct{}),
	}
	processor := newGenerationProcessor(t, messages, newMemoryCheckpoints(), newGenerationStore(t),
		"policy-a", deriver, newRecordingLane())

	deriver.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := processor.ProcessConversation(ctx, scope, conversationID)
		done <- err
	}()
	<-deriver.entered

	state, err := processor.DerivationState(ctx, scope, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.InFlight || state.Settled() {
		t.Fatalf("state mid-pass = %#v, want a pass in flight", state)
	}
	refusal := processor.RequireSettled(ctx, scope, conversationID)
	var unsettled *UnsettledDerivationError
	if !errors.As(refusal, &unsettled) {
		t.Fatalf("refusal = %v, want an unsettled derivation", refusal)
	}
	if !errors.Is(refusal, ErrDerivationUnsettled) || unsettled.ConversationID != conversationID {
		t.Fatalf("refusal = %v, want the sentinel naming %q", refusal, conversationID)
	}
	if !strings.Contains(refusal.Error(), "a derivation pass is running") {
		t.Fatalf("refusal = %v, want it to name the pass", refusal)
	}
	// The marker is per conversation and per scope: a sweep cannot race a pass
	// that is not over the conversation it retires.
	if state, err := processor.DerivationState(ctx, corememory.Scope{RuntimeID: "elsewhere"}, conversationID); err != nil {
		t.Fatal(err)
	} else if !state.Settled() {
		t.Fatalf("state of a conversation in another scope = %#v, want settled", state)
	}

	close(deriver.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state, err := processor.DerivationState(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	} else if !state.Settled() {
		t.Fatalf("state after the pass = %#v, want settled", state)
	}
}

// TestDerivationStateReportsTheGenerationAPassDerivedButNeverPublished pins the
// second half: a pass that stops between the facts it derived and the switch
// that serves them leaves work this processor still owes the conversation, and
// the next pass publishes that generation whatever a sweep did to it.
func TestDerivationStateReportsTheGenerationAPassDerivedButNeverPublished(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: {conversationCommit(conversationID, "we talked about beverages")},
	}}
	store := newGenerationStore(t)
	lane := newRecordingLane()
	checkpoints := newMemoryCheckpoints()

	published := newGenerationProcessor(t, messages, checkpoints, store, "policy-a",
		policyDeriver{prefix: "a"}, lane)
	if _, err := published.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	processor := newGenerationProcessor(t, messages, checkpoints, store, "policy-b",
		policyDeriver{prefix: "b"}, lane)
	lane.failNextConverge = errors.New("lane is down")
	if _, err := processor.ProcessConversation(ctx, scope, conversationID); err == nil {
		t.Fatal("a failed converge was reported as a successful pass")
	}
	state, err := processor.DerivationState(ctx, scope, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if state.InFlight {
		t.Fatal("the pass that failed left the conversation looking busy")
	}
	if !state.Unpublished || state.Settled() {
		t.Fatalf("state after the failed converge = %#v, want the generation the pass derived but never published", state)
	}
	refusal := processor.RequireSettled(ctx, scope, conversationID)
	if !errors.Is(refusal, ErrDerivationUnsettled) || !strings.Contains(refusal.Error(), "not published") {
		t.Fatalf("refusal = %v, want the unpublished generation", refusal)
	}

	// The pass that finishes publishes the generation, and the conversation is
	// quick to settle: nothing else is left owed.
	if _, err := processor.ProcessConversation(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	}
	if state, err := processor.DerivationState(ctx, scope, conversationID); err != nil {
		t.Fatal(err)
	} else if !state.Settled() {
		t.Fatalf("state after the pass that published = %#v, want settled", state)
	}
	if err := processor.RequireSettled(ctx, scope, conversationID); err != nil {
		t.Fatalf("a settled conversation was refused: %v", err)
	}
}

// TestDerivationStateDoesNotWaitForCommitsNoPassHasScanned pins what the guard
// deliberately does not require. A sweep is refused for work this processor has
// started, not for work it has left: a conversation whose commits no pass has
// scanned is sweepable, because a sweep only removes generations readers do not
// resolve and the pass that catches up converges the lanes with the generation
// it publishes.
func TestDerivationStateDoesNotWaitForCommitsNoPassHasScanned(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	const conversationID = "conv-a"
	messages := &fakeMessages{commits: map[string][]msgsource.Commit{
		conversationID: {conversationCommit(conversationID, "we talked about beverages")},
	}}
	processor := newGenerationProcessor(t, messages, newMemoryCheckpoints(), newGenerationStore(t),
		"policy-a", policyDeriver{prefix: "a"}, newRecordingLane())

	state, err := processor.DerivationState(ctx, scope, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Settled() {
		t.Fatalf("state before the first pass = %#v, want settled", state)
	}
	if err := processor.RequireSettled(ctx, scope, conversationID); err != nil {
		t.Fatalf("a conversation with underived commits was refused: %v", err)
	}
}
