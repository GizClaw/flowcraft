package memory

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/worker"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// blockingRepairDeriver derives like the per-input deriver until the test arms
// it, then holds every call until the test closes the release: a pass that
// reaches it stays in flight for as long as the test needs, which is how a
// repair beside a running pass is staged.
type blockingRepairDeriver struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (deriver *blockingRepairDeriver) Derive(ctx context.Context, input component.Artifact) ([]component.Artifact, error) {
	if deriver.armed.Load() {
		deriver.entered <- struct{}{}
		<-deriver.release
	}
	return perInputDeriver{}.Derive(ctx, input)
}

// longTermTexts returns the long-term items one query is answered with. The
// recent window is not part of it: the fixture derives facts whose text repeats
// the message, so only the source class tells the lanes apart.
func longTermTexts(t *testing.T, assembly *Assembly, query string) []string {
	t.Helper()
	result, err := assembly.Context(context.Background(), corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: query,
	})
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, 0)
	for _, item := range result.Items {
		if item.SourceClass == corememory.ContextSourceLongTerm {
			texts = append(texts, item.Content.Text())
		}
	}
	return texts
}

func answeredLongTerm(t *testing.T, assembly *Assembly, query string) bool {
	t.Helper()
	for _, text := range longTermTexts(t, assembly, query) {
		if strings.Contains(text, query) {
			return true
		}
	}
	return false
}

// scopeDiagnostics reads the derivation state of the fixture's single scope:
// the cursors a pass resumes from, which a rebuild must leave where they are.
func scopeDiagnostics(t *testing.T, assembly *Assembly) worker.ScopeDiagnostics {
	t.Helper()
	diagnostics, err := assembly.Diagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics.Scopes) != 1 {
		t.Fatalf("scopes = %+v", diagnostics.Scopes)
	}
	return diagnostics.Scopes[0].ScopeDiagnostics
}

func activeGeneration(t *testing.T, assembly *Assembly, conversationID string) string {
	t.Helper()
	generation, found, err := assembly.Facts().ActiveGeneration(context.Background(), testScope(), conversationID)
	if err != nil || !found {
		t.Fatalf("active generation = %q, %v, %v", generation, found, err)
	}
	return generation
}

// TestRepairConvergesAStaleLaneFromCanonicalState is the regression fixture of
// #604: state an earlier producer wrote is reported stale, does not answer the
// query the current splitter builds out of CJK text, and is converged by a
// repair that indexes canonical state again -- without re-deriving anything and
// without moving the watermarks or the published generation.
func TestRepairConvergesAStaleLaneFromCanonicalState(t *testing.T) {
	ctx := context.Background()
	assembly, ws, factID := derivedLaneAssembly(t)
	scope := testScope()
	seedStaleBM25Lane(t, ws, scope, "facts", "conv-1", factID, "你好世界")

	// The earlier producer's terms cannot be reached by the characters and
	// bigrams the current splitter tokenizes the query with.
	if answeredLongTerm(t, assembly, "你好世界") {
		t.Fatal("the stale lane answered a query its producer cannot match")
	}

	// What a re-index must leave alone: the cursors a pass resumes from, and
	// the generation readers resolve.
	derivationBefore := scopeDiagnostics(t, assembly)
	generationBefore := activeGeneration(t, assembly, "conv-1")

	result, err := assembly.Repair(ctx, RepairRequest{Scope: scope, Lanes: []string{"bm25"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Lanes) != 1 || result.Lanes[0].Name != "bm25" || !result.Lanes[0].StaleBefore {
		t.Fatalf("repair result = %+v", result)
	}
	if result.Artifacts == 0 || result.Lanes[0].Artifacts != result.Artifacts {
		t.Fatalf("repair indexed %d artifacts for %+v", result.Artifacts, result.Lanes)
	}
	if result.Lanes[0].SourceDigest == "" || result.Lanes[0].BuildDigest == "" {
		t.Fatalf("rebuilt lane reported no digests: %+v", result.Lanes[0])
	}

	// The lane answers the query again, from the state the repair indexed.
	if !answeredLongTerm(t, assembly, "你好世界") {
		t.Fatalf("repaired lane answered with %+v", longTermTexts(t, assembly, "你好世界"))
	}

	if after := scopeDiagnostics(t, assembly); !reflect.DeepEqual(after, derivationBefore) {
		t.Fatalf("the repair moved the scope's derivation state: %+v -> %+v", derivationBefore, after)
	}
	if after := activeGeneration(t, assembly, "conv-1"); after != generationBefore {
		t.Fatalf("generation moved from %q to %q", generationBefore, after)
	}

	// A pass over the conversation derives nothing: the repair indexed what was
	// derived, so the pipeline has no work it has not already done, and the
	// cursors stay where they are.
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if after := scopeDiagnostics(t, assembly); !reflect.DeepEqual(after, derivationBefore) {
		t.Fatalf("a pass after the repair moved the scope's derivation state: %+v -> %+v", derivationBefore, after)
	}

	// Rebuilding twice publishes the same state, so a retried repair is safe.
	again, err := assembly.Repair(ctx, RepairRequest{Scope: scope, Lanes: []string{"bm25"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Lanes) != 1 || again.Lanes[0].StaleBefore {
		t.Fatalf("second repair = %+v", again.Lanes)
	}
	if again.Lanes[0].SourceDigest != result.Lanes[0].SourceDigest ||
		again.Lanes[0].BuildDigest != result.Lanes[0].BuildDigest {
		t.Fatalf("rebuild was not idempotent: %+v then %+v", result.Lanes[0], again.Lanes[0])
	}

	// Nothing is stale any more: not in the lanes, and not in the plan a host
	// checks before deciding to repair.
	lanes, err := assembly.LaneStatus(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if lane := laneNamed(t, lanes, "bm25"); !lane.Stored || lane.Stale || lane.StoredProducer != lane.ExpectedProducer {
		t.Fatalf("lane after the repair = %+v", lane)
	}
	for _, lane := range lanes {
		if lane.Stale {
			t.Fatalf("lane reported stale after the repair: %+v", lane)
		}
	}
	plan, err := assembly.Verify(ctx, scope, "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range plan.Actions {
		if action.Kind == "rebuild" && action.Target == "projection:bm25" {
			t.Fatalf("verify plan still asks for a rebuild: %+v", action)
		}
	}
}

// A repair is bounded by the scope it names, and its lane list is resolved
// rather than guessed: an empty list means the lanes whose rebuild needs no
// inference, and a name that is not configured is refused.
func TestRepairSelectsConfiguredLanes(t *testing.T) {
	ctx := context.Background()
	assembly, _, _ := derivedLaneAssembly(t)
	scope := testScope()

	result, err := assembly.Repair(ctx, RepairRequest{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(result.Lanes))
	for _, lane := range result.Lanes {
		names = append(names, lane.Name)
	}
	if len(names) != 2 || names[0] != "bm25" || names[1] != "entity" {
		t.Fatalf("default repair rebuilt %v", names)
	}

	// The embedding lane is configured only with a model; naming one that is
	// not configured is a validation error, not a silent no-op.
	if _, err := assembly.Repair(ctx, RepairRequest{Scope: scope, Lanes: []string{"vector"}}); err == nil {
		t.Fatal("a repair named an unconfigured lane")
	}
	if _, err := assembly.Repair(ctx, RepairRequest{Scope: scope, Lanes: []string{"bm25", "bm25"}}); err != nil {
		t.Fatalf("a repeated lane name was refused: %v", err)
	}
}

// TestRepairRefusesWhileADerivationPassIsInFlight pins the first half of the
// guard: a rebuild reads canonical state and then replaces what the lanes hold,
// so a pass writing entries in between would be indexed into a lane the rebuild
// is about to replace. The repair refuses -- retryably, rebuilding nothing --
// and the host runs it after the pass.
func TestRepairRefusesWhileADerivationPassIsInFlight(t *testing.T) {
	ctx := context.Background()
	deriver := &blockingRepairDeriver{entered: make(chan struct{}, 4), release: make(chan struct{})}
	assembly, _ := newTestAssemblyWith(t, testSettings, WithDeriver(deriver))
	if err := assembly.CommitTurn(ctx, corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "你好世界")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// A second turn gives the pass work; it stops in the deriver, which is
	// mid-scan and long before the lanes converge.
	if err := assembly.CommitTurn(ctx, corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-2",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "还有别的")},
	}); err != nil {
		t.Fatal(err)
	}
	deriver.armed.Store(true)
	done := make(chan error, 1)
	go func() { done <- assembly.RunOnce(ctx) }()
	<-deriver.entered

	result, err := assembly.Repair(ctx, RepairRequest{Scope: testScope()})
	if err == nil {
		t.Fatal("a repair beside a running pass was accepted")
	}
	if !errors.Is(err, worker.ErrDerivationUnsettled) {
		t.Fatalf("refusal = %v, want an unsettled derivation", err)
	}
	if len(result.Lanes) != 0 || result.Artifacts != 0 {
		t.Fatalf("a refused repair rebuilt %+v", result)
	}

	close(deriver.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := assembly.Repair(ctx, RepairRequest{Scope: testScope()}); err != nil {
		t.Fatalf("repair after the pass: %v", err)
	}
}

// TestDerivationRefusesWhileTheLanesAreRepaired pins the other half of the same
// guard: while a rebuild holds the scope, a pass refuses instead of indexing
// against a lane being rewritten, and runs again once the rebuild released it.
func TestDerivationRefusesWhileTheLanesAreRepaired(t *testing.T) {
	ctx := context.Background()
	assembly, _, _ := derivedLaneAssembly(t)
	release, err := assembly.processor.BeginRepair(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assembly.processor.ProcessConversation(ctx, testScope(), "conv-1"); !errors.Is(err, worker.ErrScopeUnderRepair) {
		t.Fatalf("conversation pass during a repair = %v", err)
	}
	if err := assembly.RunOnce(ctx); !errors.Is(err, worker.ErrScopeUnderRepair) {
		t.Fatalf("scope pass during a repair = %v", err)
	}
	release()
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatalf("pass after the repair: %v", err)
	}
}
