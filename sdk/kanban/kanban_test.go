package kanban_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/kanban"
)

func newBoard(t *testing.T, opts ...kanban.Option) *kanban.Kanban {
	t.Helper()
	k := kanban.New("test-scope", opts...)
	t.Cleanup(func() { _ = k.Close() })
	return k
}

func mustSubmit(t *testing.T, k *kanban.Kanban, target string) *kanban.Card {
	t.Helper()
	card, err := k.Submit(context.Background(), kanban.Task{
		TargetAgentID: target,
		Query:         "do the thing",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return card
}

func TestSubmitCreatesPendingCard(t *testing.T) {
	k := newBoard(t)
	ctx := kanban.WithProducerID(context.Background(), "planner")

	card, err := k.Submit(ctx, kanban.Task{
		TargetAgentID: "worker",
		Query:         "summarise",
		UserQuery:     "what happened today",
		DispatchNote:  "needs the digest",
		Timeout:       30 * time.Second,
	}, kanban.WithMeta("tenant", "acme"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if card.Status != kanban.StatusPending {
		t.Errorf("Status = %q, want pending", card.Status)
	}
	if card.Producer != "planner" {
		t.Errorf("Producer = %q, want planner", card.Producer)
	}
	if card.Task == nil || card.Task.Query != "summarise" {
		t.Fatalf("Task not preserved: %+v", card.Task)
	}
	if card.Task.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", card.Task.Timeout)
	}
	if card.Meta["tenant"] != "acme" {
		t.Errorf("Meta = %v", card.Meta)
	}
	if card.Result != nil {
		t.Errorf("Result = %+v, want nil before terminal", card.Result)
	}
}

func TestSubmitRequiresTargetAgent(t *testing.T) {
	k := newBoard(t)
	_, err := k.Submit(context.Background(), kanban.Task{Query: "x"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want validation", err)
	}
}

func TestSubmitRejectsNegativeTimeout(t *testing.T) {
	k := newBoard(t)
	_, err := k.Submit(context.Background(), kanban.Task{
		TargetAgentID: "w",
		Timeout:       -time.Second,
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want validation", err)
	}
}

func TestValidatorRejectsSubmission(t *testing.T) {
	want := errdefs.NotFoundf("no such agent")
	k := newBoard(t, kanban.WithValidator(func(_ context.Context, tk kanban.Task) error {
		if tk.TargetAgentID != "known" {
			return want
		}
		return nil
	}))

	if _, err := k.Submit(context.Background(), kanban.Task{TargetAgentID: "ghost"}); err == nil {
		t.Fatal("Submit accepted an unvalidated target")
	}
	if k.Len() != 0 {
		t.Errorf("Len = %d, want 0 — a rejected submission must not leave a card", k.Len())
	}
	if _, err := k.Submit(context.Background(), kanban.Task{TargetAgentID: "known"}); err != nil {
		t.Fatalf("Submit(known): %v", err)
	}
}

func TestMaxPendingShedsLoad(t *testing.T) {
	k := newBoard(t, kanban.WithMaxPending(2))
	first := mustSubmit(t, k, "w")
	mustSubmit(t, k, "w")

	_, err := k.Submit(context.Background(), kanban.Task{TargetAgentID: "w"})
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("err = %v, want rate limit", err)
	}

	// Claiming frees a pending slot: the cap counts queue depth, not
	// total cards.
	if !k.Claim(first.ID, "worker-1") {
		t.Fatal("Claim failed")
	}
	if _, err := k.Submit(context.Background(), kanban.Task{TargetAgentID: "w"}); err != nil {
		t.Fatalf("Submit after Claim freed a slot: %v", err)
	}
}

func TestClaimIsExclusive(t *testing.T) {
	k := newBoard(t)
	card := mustSubmit(t, k, "w")

	const workers = 32
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			if k.Claim(card.ID, fmt.Sprintf("worker-%d", id)) {
				mu.Lock()
				winners = append(winners, fmt.Sprintf("worker-%d", id))
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("winners = %v, want exactly one", winners)
	}
	got, _ := k.Card(card.ID)
	if got.Consumer != winners[0] {
		t.Errorf("Consumer = %q, want %q", got.Consumer, winners[0])
	}
}

func TestTerminalTransitionsRequireClaim(t *testing.T) {
	tests := []struct {
		name string
		call func(*kanban.Kanban, string) bool
	}{
		{"Done", func(k *kanban.Kanban, id string) bool {
			return k.Done(id, kanban.Result{Output: "x"})
		}},
		{"Fail", func(k *kanban.Kanban, id string) bool {
			return k.Fail(id, "boom")
		}},
		{"Suspend", func(k *kanban.Kanban, id string) bool {
			return k.Suspend(id, "ckpt")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k := newBoard(t)
			card := mustSubmit(t, k, "w")
			if tc.call(k, card.ID) {
				t.Fatalf("%s succeeded on a pending card", tc.name)
			}
			if !k.Claim(card.ID, "worker") {
				t.Fatal("Claim failed")
			}
			if !tc.call(k, card.ID) {
				t.Fatalf("%s failed on a claimed card", tc.name)
			}
		})
	}
}

func TestDoneIsTerminal(t *testing.T) {
	k := newBoard(t)
	card := mustSubmit(t, k, "w")
	k.Claim(card.ID, "worker")
	if !k.Done(card.ID, kanban.Result{Output: "result"}) {
		t.Fatal("Done failed")
	}

	got, _ := k.Card(card.ID)
	if got.Status != kanban.StatusDone || got.Result.Output != "result" {
		t.Fatalf("card = %+v", got)
	}
	if k.Done(card.ID, kanban.Result{Output: "again"}) {
		t.Error("Done applied twice")
	}
	if k.Cancel(card.ID, "too late") {
		t.Error("Cancel applied to a terminal card")
	}
}

func TestSuspendReleasesWorkerAndResumeReturnsRef(t *testing.T) {
	k := newBoard(t)
	card := mustSubmit(t, k, "w")
	k.Claim(card.ID, "worker-1")

	if !k.Suspend(card.ID, "checkpoint-42") {
		t.Fatal("Suspend failed")
	}
	got, _ := k.Card(card.ID)
	if got.Status != kanban.StatusSuspended {
		t.Errorf("Status = %q, want suspended", got.Status)
	}
	if got.Consumer != "" {
		t.Errorf("Consumer = %q, want cleared — a suspended card holds no worker", got.Consumer)
	}
	if got.ResumeRef != "checkpoint-42" {
		t.Errorf("ResumeRef = %q", got.ResumeRef)
	}
	if got.Status.IsTerminal() {
		t.Error("suspended must not be terminal")
	}

	ref, ok := k.Resume(card.ID)
	if !ok || ref != "checkpoint-42" {
		t.Fatalf("Resume = (%q, %v)", ref, ok)
	}
	got, _ = k.Card(card.ID)
	if got.Status != kanban.StatusPending {
		t.Errorf("Status = %q, want pending after resume", got.Status)
	}
	if got.ResumeRef != "checkpoint-42" {
		t.Errorf("ResumeRef = %q, want retained after resume", got.ResumeRef)
	}

	// A resumed card is claimable again, by a different worker.
	if !k.Claim(card.ID, "worker-2") {
		t.Fatal("Claim after Resume failed")
	}
}

func TestResumeRequiresSuspended(t *testing.T) {
	k := newBoard(t)
	card := mustSubmit(t, k, "w")
	if _, ok := k.Resume(card.ID); ok {
		t.Error("Resume succeeded on a pending card")
	}
	k.Claim(card.ID, "worker")
	if _, ok := k.Resume(card.ID); ok {
		t.Error("Resume succeeded on a claimed card")
	}
}

func TestCancelFromEveryNonTerminalState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*kanban.Kanban, string)
	}{
		{"pending", func(*kanban.Kanban, string) {}},
		{"claimed", func(k *kanban.Kanban, id string) { k.Claim(id, "w") }},
		{"suspended", func(k *kanban.Kanban, id string) {
			k.Claim(id, "w")
			k.Suspend(id, "ref")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newBoard(t)
			card := mustSubmit(t, k, "w")
			tc.setup(k, card.ID)
			if !k.Cancel(card.ID, "shutting down") {
				t.Fatalf("Cancel failed from %s", tc.name)
			}
			got, _ := k.Card(card.ID)
			if got.Status != kanban.StatusCancelled {
				t.Errorf("Status = %q", got.Status)
			}
			if got.Result.Error != "shutting down" {
				t.Errorf("Result.Error = %q", got.Result.Error)
			}
		})
	}
}

func TestTransitionsOnUnknownCard(t *testing.T) {
	k := newBoard(t)
	if k.Claim("nope", "w") || k.Done("nope", kanban.Result{}) ||
		k.Fail("nope", "e") || k.Cancel("nope", "r") || k.Suspend("nope", "x") {
		t.Error("a transition succeeded on a nonexistent card")
	}
	if _, ok := k.Resume("nope"); ok {
		t.Error("Resume succeeded on a nonexistent card")
	}
}

func TestQueryFilters(t *testing.T) {
	k := newBoard(t)
	ctx := kanban.WithProducerID(context.Background(), "planner")
	a, _ := k.Submit(ctx, kanban.Task{TargetAgentID: "alpha"})
	k.Submit(ctx, kanban.Task{TargetAgentID: "beta"})
	k.Claim(a.ID, "worker-1")

	if got := len(k.Query(kanban.Filter{})); got != 2 {
		t.Errorf("zero Filter matched %d, want 2 (wildcard)", got)
	}
	if got := k.Query(kanban.Filter{TargetAgentID: "beta"}); len(got) != 1 {
		t.Errorf("TargetAgentID filter matched %d, want 1", len(got))
	}
	if got := k.Query(kanban.Filter{Status: kanban.StatusClaimed}); len(got) != 1 ||
		got[0].ID != a.ID {
		t.Errorf("Status filter = %+v", got)
	}
	if got := k.Query(kanban.Filter{Consumer: "worker-1"}); len(got) != 1 {
		t.Errorf("Consumer filter matched %d, want 1", len(got))
	}
	if got := k.Query(kanban.Filter{Producer: "planner"}); len(got) != 2 {
		t.Errorf("Producer filter matched %d, want 2", len(got))
	}
}

func TestQueryReturnsCopies(t *testing.T) {
	k := newBoard(t)
	card, _ := k.Submit(context.Background(), kanban.Task{
		TargetAgentID: "w",
		Query:         "original",
		Inputs:        map[string]any{"n": 1},
	}, kanban.WithMeta("k", "v"))

	got, _ := k.Card(card.ID)
	got.Task.Query = "mutated"
	got.Meta["k"] = "mutated"
	got.Task.Inputs["n"] = 99

	fresh, _ := k.Card(card.ID)
	if fresh.Task.Query != "original" {
		t.Error("mutating a returned card changed board state")
	}
	if fresh.Meta["k"] != "v" {
		t.Error("mutating returned Meta changed board state")
	}
	if fresh.Task.Inputs["n"] != 1 {
		t.Error("mutating returned Inputs changed board state")
	}
}

func TestSubmitCopiesInputs(t *testing.T) {
	k := newBoard(t)
	inputs := map[string]any{"n": 1}
	card, _ := k.Submit(context.Background(), kanban.Task{
		TargetAgentID: "w",
		Inputs:        inputs,
	})
	inputs["n"] = 99

	got, _ := k.Card(card.ID)
	if got.Task.Inputs["n"] != 1 {
		t.Error("mutating the caller's map after Submit changed the card")
	}
}

func TestCountByStatusTracksTransitions(t *testing.T) {
	k := newBoard(t)
	a := mustSubmit(t, k, "w")
	b := mustSubmit(t, k, "w")

	if got := k.CountByStatus(kanban.StatusPending); got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}
	k.Claim(a.ID, "worker")
	k.Done(a.ID, kanban.Result{})
	if got := k.CountByStatus(kanban.StatusPending); got != 1 {
		t.Errorf("pending = %d, want 1", got)
	}
	if got := k.CountByStatus(kanban.StatusDone); got != 1 {
		t.Errorf("done = %d, want 1", got)
	}
	k.Claim(b.ID, "worker")
	k.Suspend(b.ID, "ref")
	if got := k.CountByStatus(kanban.StatusSuspended); got != 1 {
		t.Errorf("suspended = %d, want 1", got)
	}
	if got := k.CountByStatus(kanban.StatusClaimed); got != 0 {
		t.Errorf("claimed = %d, want 0", got)
	}
}

func TestEvictionSparesNonTerminalCards(t *testing.T) {
	k := newBoard(t, kanban.WithMaxCards(1))
	done := mustSubmit(t, k, "w")
	k.Claim(done.ID, "worker")
	k.Done(done.ID, kanban.Result{})

	pending := mustSubmit(t, k, "w")
	suspended := mustSubmit(t, k, "w")
	k.Claim(suspended.ID, "worker")
	k.Suspend(suspended.ID, "ref")

	// Force an eviction pass.
	mustSubmit(t, k, "w")

	if _, ok := k.Card(pending.ID); !ok {
		t.Error("evicted a pending card — outstanding work must survive")
	}
	if _, ok := k.Card(suspended.ID); !ok {
		t.Error("evicted a suspended card — outstanding work must survive")
	}
	if _, ok := k.Card(done.ID); ok {
		t.Error("terminal card survived the cap")
	}
}

func TestCardTTLEvictsOnlyTerminal(t *testing.T) {
	k := newBoard(t, kanban.WithCardTTL(time.Nanosecond))
	old := mustSubmit(t, k, "w")
	k.Claim(old.ID, "worker")
	k.Done(old.ID, kanban.Result{})
	pending := mustSubmit(t, k, "w")

	time.Sleep(2 * time.Millisecond)
	mustSubmit(t, k, "w") // triggers the eviction pass

	if _, ok := k.Card(old.ID); ok {
		t.Error("expired terminal card survived")
	}
	if _, ok := k.Card(pending.ID); !ok {
		t.Error("TTL evicted a pending card")
	}
}

func TestSubmitAfterCloseFails(t *testing.T) {
	k := kanban.New("scope")
	if err := k.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := k.Submit(context.Background(), kanban.Task{TargetAgentID: "w"})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want not available", err)
	}
	if err := k.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestContextCancelledOnClose(t *testing.T) {
	k := kanban.New("scope")
	if k.Context().Err() != nil {
		t.Fatal("Context cancelled before Close")
	}
	_ = k.Close()
	if k.Context().Err() == nil {
		t.Error("Context not cancelled by Close")
	}
}

func TestElapsedIsMonotonic(t *testing.T) {
	k := newBoard(t)
	card := mustSubmit(t, k, "w")
	if got := card.Elapsed(); got < 0 {
		t.Errorf("Elapsed = %v, want >= 0", got)
	}
	time.Sleep(2 * time.Millisecond)
	k.Claim(card.ID, "worker")
	k.Done(card.ID, kanban.Result{})
	got, _ := k.Card(card.ID)
	if got.Elapsed() <= 0 {
		t.Errorf("Elapsed = %v, want > 0 after a delayed transition", got.Elapsed())
	}
}
