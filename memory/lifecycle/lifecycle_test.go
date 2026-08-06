package lifecycle

import (
	"context"
	"testing"
	"time"

	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type fixedClock struct{ value time.Time }

func (clock fixedClock) Now() time.Time { return clock.value }

func TestDecayFormulaFutureTimeAndMonotonicity(t *testing.T) {
	now := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	decay, err := NewDecay(DecayConfig{
		Version: DecayAlgorithmVersion, HalfLife: 24 * time.Hour,
		RecencyWeight: .5, FrequencyWeight: .3, RelevanceWeight: .2,
	}, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	recent := decay.Score(DecayInput{LastAccessAt: now, AccessCount: 4, Relevance: .8})
	old := decay.Score(DecayInput{LastAccessAt: now.Add(-24 * time.Hour), AccessCount: 4, Relevance: .8})
	future := decay.Score(DecayInput{LastAccessAt: now.Add(time.Hour), AccessCount: 4, Relevance: 2})
	if !(recent.Score > old.Score) || future.Recency != 1 || future.Relevance != 1 {
		t.Fatalf("scores recent=%#v old=%#v future=%#v", recent, old, future)
	}
}

func TestDecayUsesDurableBaseTimeBeforeFirstRecall(t *testing.T) {
	now := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	decay, err := NewDecay(DecayConfig{
		HalfLife: 24 * time.Hour, RecencyWeight: .8, RelevanceWeight: .2,
	}, fixedClock{now})
	if err != nil {
		t.Fatal(err)
	}
	unrecalled := decay.Score(DecayInput{BaseTime: now.Add(-72 * time.Hour)})
	if unrecalled.Recency >= .2 || unrecalled.Score >= .2 {
		t.Fatalf("unrecalled fact was treated as new/relevant: %#v", unrecalled)
	}
	if unrecalled.AlgorithmVersion != DecayAlgorithmVersion {
		t.Fatalf("untyped decay version %q", unrecalled.AlgorithmVersion)
	}
}

func TestOutboxRestartAndStaleLease(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	clock := &fixedClock{value: now}
	ws := workspace.NewMemWorkspace()
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	outbox, _ := NewWorkspaceOutbox(ws, clock)
	task := Task{Scope: scope, FactID: "fact", StateEvent: EventFactPublished, PublicationID: "publication",
		RevisionDigest: "revision", PolicyDigest: "policy", Branch: "integrate"}
	first, err := outbox.Enqueue(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	reopened, _ := NewWorkspaceOutbox(ws, clock)
	retry, err := reopened.Enqueue(ctx, task)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("retry = %#v, %v", retry, err)
	}
	lease1, ok, err := reopened.LeaseNext(ctx, scope, "one", time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease1 = %#v, %v, %v", lease1, ok, err)
	}
	clock.value = now.Add(2 * time.Minute)
	lease2, ok, err := reopened.LeaseNext(ctx, scope, "two", time.Minute)
	if err != nil || !ok || lease2.Token == lease1.Token {
		t.Fatalf("lease2 = %#v, %v, %v", lease2, ok, err)
	}
	if err := reopened.Complete(ctx, first.ID, lease1.Token); err == nil {
		t.Fatal("stale lease completed")
	}
	if err := reopened.Complete(ctx, first.ID, lease2.Token); err != nil {
		t.Fatal(err)
	}
}

func TestOutboxRenewAndSequenceSurviveClockRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	clock := &fixedClock{value: now}
	outbox, _ := NewWorkspaceOutbox(workspace.NewMemWorkspace(), clock)
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	task, err := outbox.Enqueue(ctx, Task{
		Scope: scope, FactID: "fact", StateEvent: EventFactPublished,
		PublicationID: "publication", RevisionDigest: "revision", PolicyDigest: "policy", Branch: "integrate",
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := outbox.LeaseNext(ctx, scope, "runner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	clock.value = now.Add(30 * time.Second)
	expiresAt, err := outbox.Renew(ctx, task.ID, lease.Token, time.Minute)
	if err != nil || !expiresAt.Equal(clock.value.Add(time.Minute)) {
		t.Fatalf("renew expiry=%v err=%v", expiresAt, err)
	}
	clock.value = now.Add(-time.Hour)
	if err := outbox.Fail(ctx, task.ID, lease.Token, context.Canceled); err != nil {
		t.Fatal(err)
	}
	retry, ok, err := outbox.LeaseNext(ctx, scope, "retry", time.Minute)
	if err != nil || !ok || retry.Token == lease.Token {
		t.Fatalf("clock rollback hid latest sequence: %#v, %v, %v", retry, ok, err)
	}
	if err := outbox.Complete(ctx, task.ID, lease.Token); err == nil {
		t.Fatal("stale pre-retry token completed")
	}
}

func TestFactMergeCreatesDistinctPublicationTaskAndReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	clock := &fixedClock{value: now}
	ws := workspace.NewMemWorkspace()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	outbox, _ := NewWorkspaceOutbox(ws, clock)
	sink := &OutboxSink{Outbox: outbox, PolicyDigest: "policy", Branch: "integrate"}
	facts, _ := factview.NewWorkspaceStore(ws, factview.WithClock(clock.Now), factview.WithPublicationSink(sink))
	base := factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation",
		Content:  sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "stable body"}}},
		Entities: []string{"stable"}, LinkedMemoryIDs: []string{"related"},
		Provenance: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "message-a"}},
	}
	if _, err := facts.Add(ctx, base); err != nil {
		t.Fatal(err)
	}
	first, ok, err := outbox.LeaseNext(ctx, scope, "runner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("initial publication lease = %#v, %v, %v", first, ok, err)
	}
	if err := outbox.Complete(ctx, first.Task.ID, first.Token); err != nil {
		t.Fatal(err)
	}

	merge := base
	merge.ID = "same-canonical-content"
	merge.Provenance = []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "message-b"}}
	if _, err := facts.Add(ctx, merge); err != nil {
		t.Fatal(err)
	}
	second, ok, err := outbox.LeaseNext(ctx, scope, "runner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("merge publication was swallowed by completed task: %#v, %v, %v", second, ok, err)
	}
	if second.Task.ID == first.Task.ID {
		t.Fatalf("merge reused initial publication task %q", first.Task.ID)
	}
	if err := outbox.Complete(ctx, second.Task.ID, second.Token); err != nil {
		t.Fatal(err)
	}
	thirdMerge := base
	thirdMerge.ID = "third-canonical-content"
	thirdMerge.Provenance = []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "message-c"}}
	if _, err := facts.Add(ctx, thirdMerge); err != nil {
		t.Fatal(err)
	}
	third, ok, err := outbox.LeaseNext(ctx, scope, "runner", time.Minute)
	if err != nil || !ok || third.Task.ID == second.Task.ID {
		t.Fatalf("subsequent merge publication was swallowed: %#v, %v, %v", third, ok, err)
	}
	if err := outbox.Complete(ctx, third.Task.ID, third.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := facts.Add(ctx, thirdMerge); err != nil {
		t.Fatal(err)
	}
	if replay, ok, err := outbox.LeaseNext(ctx, scope, "runner", time.Minute); err != nil || ok {
		t.Fatalf("same merge replay created work: %#v, %v, %v", replay, ok, err)
	}
}

func TestReinforceIdempotentAndForgetSafety(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 4, 0, 0, 0, time.UTC)
	store, _ := NewWorkspaceEventStore(workspace.NewMemWorkspace())
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	event := sdkmemory.RecallEvent{ID: "invocation-1", Scope: scope, ItemIDs: []string{"a"}, Scores: []float64{.7}, Time: now}
	if err := store.RecordRecall(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRecall(ctx, event); err != nil {
		t.Fatal(err)
	}
	aggregate, ok, err := store.Access(ctx, scope, "a")
	if err != nil || !ok || aggregate.AccessCount != 1 || aggregate.LastRecallScore != .7 {
		t.Fatalf("aggregate = %#v, %v, %v", aggregate, ok, err)
	}
	audit := PlanForget(ForgetConfig{Mode: ModeAuditOnly, SoftForgetThreshold: .2}, []ScoreSnapshot{{ObservationID: "a", Score: .1}}, now)
	if len(audit.Candidates) != 1 || audit.Candidates[0].Apply {
		t.Fatalf("audit plan = %#v", audit)
	}
	soft := PlanForget(ForgetConfig{Mode: ModeSoftVisibility, EnableSoftVisibility: true, SoftForgetThreshold: .2}, []ScoreSnapshot{{ObservationID: "a", Score: .1}}, now)
	if !soft.Candidates[0].Apply {
		t.Fatalf("soft plan = %#v", soft)
	}
}

func TestRepairFindsDanglingSummaryAndProjectionEvidence(t *testing.T) {
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	plan := InspectRepair(scope, RepairInput{
		Facts: []FactEvidence{{ID: "a", LinkedIDs: []string{"missing"}}},
		Summaries: []SummaryEvidence{{
			ID: "s", Level: 0, InputKind: SummaryInputFact, InputIDs: []string{"missing"},
			SourceDigest: "bad", ComputedSourceDigest: "good",
		}},
		Projections: []ProjectionEvidence{{
			Name: "vector", StoredBuildDigest: "bad", ComputedBuildDigest: "good",
		}},
	})
	if len(plan.Actions) != 3 || plan.ID == "" {
		t.Fatalf("repair plan = %#v", plan)
	}
	again := InspectRepair(scope, RepairInput{
		Facts: []FactEvidence{{ID: "a", LinkedIDs: []string{"missing"}}},
		Summaries: []SummaryEvidence{{
			ID: "s", Level: 0, InputKind: SummaryInputFact, InputIDs: []string{"missing"},
			SourceDigest: "bad", ComputedSourceDigest: "good",
		}},
		Projections: []ProjectionEvidence{{
			Name: "vector", StoredBuildDigest: "bad", ComputedBuildDigest: "good",
		}},
	})
	if again.ID != plan.ID {
		t.Fatalf("unstable plan id %q != %q", again.ID, plan.ID)
	}
	store, err := NewWorkspaceRepairAuditStore(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), again); err != nil {
		t.Fatalf("stable repair plan replay conflicted: %v", err)
	}
}

func TestRepairAcceptsCompleteMultilevelSummaryAndMatchingProjectionDigests(t *testing.T) {
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	plan := InspectRepair(scope, RepairInput{
		Facts: []FactEvidence{{ID: "fact-a"}, {ID: "fact-b"}},
		Summaries: []SummaryEvidence{
			{
				ID: "l0-a", Level: 0, InputKind: SummaryInputFact, InputIDs: []string{"fact-a"},
				CoverageValid: true, SourceDigest: "l0-a", ComputedSourceDigest: "l0-a",
			},
			{
				ID: "l0-b", Level: 0, InputKind: SummaryInputFact, InputIDs: []string{"fact-b"},
				CoverageValid: true, SourceDigest: "l0-b", ComputedSourceDigest: "l0-b",
			},
			{
				ID: "l1", Level: 1, InputKind: SummaryInputSummary, InputIDs: []string{"l0-a", "l0-b"},
				CoverageValid: true, SourceDigest: "l1", ComputedSourceDigest: "l1",
			},
			{
				ID: "l2", Level: 2, InputKind: SummaryInputSummary, InputIDs: []string{"l1"},
				CoverageValid: true, SourceDigest: "l2", ComputedSourceDigest: "l2",
			},
		},
		Projections: []ProjectionEvidence{{
			Name:                 "vector",
			StoredSourceDigest:   "source",
			ComputedSourceDigest: "source",
			StoredBuildDigest:    "build",
			ComputedBuildDigest:  "build",
		}},
	})
	if len(plan.Actions) != 0 {
		t.Fatalf("complete evidence produced repair actions: %#v", plan.Actions)
	}
}

func TestRepairRebuildsMissingOrSelfReferentialSummaryInputsAndTamperedProjection(t *testing.T) {
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	plan := InspectRepair(scope, RepairInput{
		Facts: []FactEvidence{{ID: "fact"}},
		Summaries: []SummaryEvidence{
			{
				ID: "missing-child", Level: 1, InputKind: SummaryInputSummary, InputIDs: []string{"absent"},
				CoverageValid: true, SourceDigest: "same", ComputedSourceDigest: "same",
			},
			{
				ID: "self", Level: 2, InputKind: SummaryInputSummary, InputIDs: []string{"self"},
				CoverageValid: true, SourceDigest: "same", ComputedSourceDigest: "same",
			},
		},
		Projections: []ProjectionEvidence{{
			Name:                 "vector",
			StoredSourceDigest:   "tampered-source",
			ComputedSourceDigest: "source",
			StoredBuildDigest:    "tampered-build",
			ComputedBuildDigest:  "build",
		}},
	})
	if len(plan.Actions) != 3 {
		t.Fatalf("repair actions = %#v, want two summaries and one projection", plan.Actions)
	}
}
