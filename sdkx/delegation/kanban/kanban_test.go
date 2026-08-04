package kanban_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/delegation"
	"github.com/GizClaw/flowcraft/sdkx/delegation/kanban"
)

func newBoard(t *testing.T, options ...kanban.Option) *kanban.Board {
	t.Helper()
	board := kanban.New("test-backend", options...)
	t.Cleanup(func() { _ = board.Close() })
	return board
}

func request(target string) delegation.AsyncRequest {
	return delegation.AsyncRequest{
		Request: sdkdelegation.Request{
			Mode:   sdkdelegation.ModeAsync,
			Target: target,
			Input:  "do the work",
			Metadata: map[string]string{
				"tenant": "acme",
			},
		},
		Caller: "planner",
		Depth:  2,
	}
}

func submit(t *testing.T, board *kanban.Board, target string) string {
	t.Helper()
	id, err := board.Submit(context.Background(), request(target))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return id
}

func TestAsyncBackendSubmitAndStatus(t *testing.T) {
	board := newBoard(t)
	req := request("worker")
	req.Request.IdempotencyKey = "delivery-1"
	id, err := board.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if id == "" {
		t.Fatal("Submit returned an empty id")
	}

	req.Request.Metadata["tenant"] = "mutated"
	card, ok := board.Card(id)
	if !ok {
		t.Fatal("Card not retained")
	}
	if card.Task.Request.Request.Target != "worker" ||
		card.Task.Request.Request.IdempotencyKey != "delivery-1" ||
		card.Task.Request.Request.Metadata["tenant"] != "acme" ||
		card.Producer != "planner" {
		t.Fatalf("typed request not preserved: %+v", card)
	}
	response, err := board.Status(context.Background(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if response.ID != id || response.Status != sdkdelegation.StatusAccepted {
		t.Fatalf("Status = %+v", response)
	}
}

func TestAsyncBackendSubmitIdempotency(t *testing.T) {
	board := newBoard(t)
	req := request("worker")
	req.Request.IdempotencyKey = "delivery-1"
	first, err := board.Submit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := board.Submit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != first || board.Len() != 1 {
		t.Fatalf("replay = %q, first = %q, cards = %d", replayed, first, board.Len())
	}

	conflict := req
	conflict.Request.Input = "different work"
	if _, err := board.Submit(context.Background(), conflict); !errdefs.IsConflict(err) {
		t.Fatalf("different request with reused key error = %v, want conflict", err)
	}
}

func TestAsyncBackendSubmitIdempotencyIsConcurrent(t *testing.T) {
	board := newBoard(t)
	req := request("worker")
	req.Request.IdempotencyKey = "delivery-concurrent"
	const callers = 64
	start := make(chan struct{})
	ids := make(chan string, callers)
	errs := make(chan error, callers)
	var waiters sync.WaitGroup
	for range callers {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			<-start
			id, err := board.Submit(context.Background(), req)
			ids <- id
			errs <- err
		}()
	}
	close(start)
	waiters.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var want string
	for id := range ids {
		if want == "" {
			want = id
		}
		if id != want {
			t.Fatalf("concurrent IDs include %q and %q", want, id)
		}
	}
	if board.Len() != 1 {
		t.Fatalf("concurrent submissions retained %d cards, want 1", board.Len())
	}
}

func TestSubmitValidationAndValidator(t *testing.T) {
	board := newBoard(t, kanban.WithValidator(func(
		_ context.Context,
		req delegation.AsyncRequest,
	) error {
		if req.Request.Target == "denied" {
			return errdefs.Forbiddenf("target denied")
		}
		return nil
	}))
	invalid := request("")
	if _, err := board.Submit(context.Background(), invalid); !errdefs.IsValidation(err) {
		t.Fatalf("invalid request error = %v", err)
	}
	syncRequest := request("worker")
	syncRequest.Request.Mode = sdkdelegation.ModeSync
	if _, err := board.Submit(context.Background(), syncRequest); !errdefs.IsValidation(err) {
		t.Fatalf("sync request error = %v", err)
	}
	if _, err := board.Submit(context.Background(), request("denied")); !errdefs.IsForbidden(err) {
		t.Fatalf("validator error = %v", err)
	}
	if board.Len() != 0 {
		t.Fatalf("rejected submissions retained %d cards", board.Len())
	}
}

func TestWorkSourceClaimBlocksThenCompletes(t *testing.T) {
	board := newBoard(t)
	claimed := make(chan delegation.Work, 1)
	go func() {
		work, err := board.Claim(context.Background())
		if err == nil {
			claimed <- work
		}
	}()

	select {
	case <-claimed:
		t.Fatal("Claim returned without pending work")
	case <-time.After(20 * time.Millisecond):
	}

	id := submit(t, board, "worker")
	var work delegation.Work
	select {
	case work = <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("Claim did not wake after Submit")
	}
	if work.ID != id || work.Request.Request.Target != "worker" {
		t.Fatalf("Claim = %+v", work)
	}
	if work.Context == nil || work.Context.Err() != nil {
		t.Fatalf("Claim context = %v, want active lease", work.Context)
	}
	running, err := board.Status(context.Background(), id)
	if err != nil || running.Status != sdkdelegation.StatusRunning {
		t.Fatalf("running Status = %+v, %v", running, err)
	}

	done := sdkdelegation.Response{
		Status: sdkdelegation.StatusSucceeded,
		Output: "finished",
		Metadata: map[string]string{
			"run": "one",
		},
	}
	if err := board.Complete(context.Background(), id, work.LeaseToken, done); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := board.Status(context.Background(), id)
	if err != nil {
		t.Fatalf("Status(done): %v", err)
	}
	if got.ID != id || got.Status != sdkdelegation.StatusSucceeded ||
		got.Output != "finished" || got.Metadata["run"] != "one" {
		t.Fatalf("terminal Status = %+v", got)
	}
	select {
	case <-work.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("Complete did not cancel the claim lease")
	}
}

func TestClaimLeaseCanceledByCancelAndClose(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		board := newBoard(t)
		id := submit(t, board, "worker")
		work, err := board.Claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !board.Cancel(id, "operator canceled") {
			t.Fatal("Cancel returned false")
		}
		select {
		case <-work.Context.Done():
		case <-time.After(time.Second):
			t.Fatal("Cancel did not cancel the claim lease")
		}
		if err := board.Complete(context.Background(), id, work.LeaseToken, sdkdelegation.Response{
			Status: sdkdelegation.StatusCanceled,
			Error:  context.Canceled.Error(),
		}); err != nil {
			t.Fatalf("canceled worker completion should be idempotent: %v", err)
		}
	})

	t.Run("close", func(t *testing.T) {
		board := kanban.New("lease-close")
		id := submit(t, board, "worker")
		work, err := board.Claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if work.ID != id {
			t.Fatalf("Claim ID = %q, want %q", work.ID, id)
		}
		if err := board.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-work.Context.Done():
		case <-time.After(time.Second):
			t.Fatal("Close did not cancel the claim lease")
		}
		if err := board.Complete(
			context.Background(),
			id,
			work.LeaseToken,
			sdkdelegation.Response{Status: sdkdelegation.StatusSucceeded},
		); err != nil {
			t.Fatalf("stale completion after Close: %v", err)
		}
	})
}

func TestClaimLeaseConcurrentCancelComplete(t *testing.T) {
	for range 100 {
		board := kanban.New("lease-race")
		id := submit(t, board, "worker")
		work, err := board.Claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		var calls sync.WaitGroup
		calls.Add(2)
		go func() {
			defer calls.Done()
			<-start
			board.Cancel(id, "raced")
		}()
		go func() {
			defer calls.Done()
			<-start
			_ = board.Complete(context.Background(), id, work.LeaseToken, sdkdelegation.Response{
				Status: sdkdelegation.StatusSucceeded,
				Output: "done",
			})
		}()
		close(start)
		calls.Wait()
		select {
		case <-work.Context.Done():
		case <-time.After(time.Second):
			t.Fatal("terminal transition did not cancel the claim lease")
		}
		if err := board.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatusMapsEveryCardState(t *testing.T) {
	tests := []struct {
		name       string
		drive      func(*kanban.Board, string)
		want       sdkdelegation.Status
		wantOutput string
		wantError  string
	}{
		{"pending", func(*kanban.Board, string) {}, sdkdelegation.StatusAccepted, "", ""},
		{"claimed", func(board *kanban.Board, id string) {
			board.ClaimCard(id, "worker")
		}, sdkdelegation.StatusRunning, "", ""},
		{"suspended", func(board *kanban.Board, id string) {
			board.ClaimCard(id, "worker")
			board.Suspend(id, "checkpoint")
		}, sdkdelegation.StatusAccepted, "", ""},
		{"done", func(board *kanban.Board, id string) {
			work, _ := board.Claim(context.Background())
			_ = board.Complete(context.Background(), id, work.LeaseToken, sdkdelegation.Response{
				Status: sdkdelegation.StatusSucceeded,
				Output: "ok",
			})
		}, sdkdelegation.StatusSucceeded, "ok", ""},
		{"failed", func(board *kanban.Board, id string) {
			work, _ := board.Claim(context.Background())
			_ = board.Complete(context.Background(), id, work.LeaseToken, sdkdelegation.Response{
				Status: sdkdelegation.StatusFailed,
				Error:  "boom",
			})
		}, sdkdelegation.StatusFailed, "", "boom"},
		{"cancelled", func(board *kanban.Board, id string) {
			board.Cancel(id, "stopped")
		}, sdkdelegation.StatusCanceled, "", "stopped"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			board := newBoard(t)
			id := submit(t, board, "worker")
			test.drive(board, id)
			response, err := board.Status(context.Background(), id)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if response.Status != test.want ||
				response.Output != test.wantOutput ||
				response.Error != test.wantError {
				t.Fatalf("Status = %+v", response)
			}
		})
	}
}

func TestClaimCardHasSingleWinner(t *testing.T) {
	board := newBoard(t)
	id := submit(t, board, "worker")
	const contenders = 64
	start := make(chan struct{})
	var (
		waiters sync.WaitGroup
		lock    sync.Mutex
		winners []string
	)
	for index := range contenders {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			consumer := fmt.Sprintf("worker-%d", index)
			<-start
			if board.ClaimCard(id, consumer) {
				lock.Lock()
				winners = append(winners, consumer)
				lock.Unlock()
			}
		}()
	}
	close(start)
	waiters.Wait()
	if len(winners) != 1 {
		t.Fatalf("claim winners = %v", winners)
	}
	card, _ := board.Card(id)
	if card.Consumer != winners[0] {
		t.Fatalf("Consumer = %q, winner = %q", card.Consumer, winners[0])
	}
}

func TestConcurrentWorkSourceClaimsDoNotDuplicateOrLoseWork(t *testing.T) {
	board := newBoard(t)
	const total = 100
	for range total {
		submit(t, board, "worker")
	}

	seen := make(map[string]bool, total)
	var lock sync.Mutex
	var waiters sync.WaitGroup
	for range 12 {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			for {
				lock.Lock()
				done := len(seen) == total
				lock.Unlock()
				if done {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				work, err := board.Claim(ctx)
				cancel()
				if err != nil {
					continue
				}
				lock.Lock()
				if seen[work.ID] {
					t.Errorf("duplicate claim for %s", work.ID)
				}
				seen[work.ID] = true
				lock.Unlock()
			}
		}()
	}
	waiters.Wait()
	if len(seen) != total {
		t.Fatalf("claimed %d cards, want %d", len(seen), total)
	}
}

func TestClaimCancellationAndClose(t *testing.T) {
	t.Run("context", func(t *testing.T) {
		board := newBoard(t)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := board.Claim(ctx)
			result <- err
		}()
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Claim error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Claim ignored context cancellation")
		}
	})
	t.Run("close", func(t *testing.T) {
		board := kanban.New("close-test")
		result := make(chan error, 1)
		go func() {
			_, err := board.Claim(context.Background())
			result <- err
		}()
		if err := board.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		select {
		case err := <-result:
			if !errdefs.IsNotAvailable(err) {
				t.Fatalf("Claim error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not wake Claim")
		}
		if _, err := board.Submit(context.Background(), request("worker")); !errdefs.IsNotAvailable(err) {
			t.Fatalf("Submit after Close error = %v", err)
		}
	})
}

func TestSuspendResumeReturnsWorkToBlockingClaim(t *testing.T) {
	board := newBoard(t)
	id := submit(t, board, "worker")
	first, err := board.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !board.Suspend(id, "checkpoint-1") {
		t.Fatal("failed to suspend claimed card")
	}
	select {
	case <-first.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("Suspend did not cancel the current lease")
	}
	response, _ := board.Status(context.Background(), id)
	if response.Status != sdkdelegation.StatusAccepted {
		t.Fatalf("suspended status = %q", response.Status)
	}

	claimed := make(chan delegation.Work, 1)
	go func() {
		work, err := board.Claim(context.Background())
		if err == nil {
			claimed <- work
		}
	}()
	select {
	case <-claimed:
		t.Fatal("suspended work was claimable")
	case <-time.After(20 * time.Millisecond):
	}
	ref, ok := board.Resume(id)
	if !ok || ref != "checkpoint-1" {
		t.Fatalf("Resume = (%q, %v)", ref, ok)
	}
	select {
	case work := <-claimed:
		if work.ID != id || work.LeaseToken == first.LeaseToken {
			t.Fatalf("reclaimed work = %+v, first token = %q", work, first.LeaseToken)
		}
	case <-time.After(time.Second):
		t.Fatal("Resume did not wake Claim")
	}
}

func TestStaleCompleteDoesNotAffectReclaimedLease(t *testing.T) {
	for range 100 {
		board := newBoard(t)
		id := submit(t, board, "worker")
		first, err := board.Claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !board.Suspend(id, "checkpoint") {
			t.Fatal("Suspend returned false")
		}
		if _, ok := board.Resume(id); !ok {
			t.Fatal("Resume returned false")
		}
		current, err := board.Claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if current.LeaseToken == first.LeaseToken {
			t.Fatal("reclaim reused lease token")
		}

		start := make(chan struct{})
		results := make(chan error, 2)
		for _, response := range []sdkdelegation.Response{
			{Status: sdkdelegation.StatusCanceled, Error: context.Canceled.Error()},
			{Status: sdkdelegation.StatusSucceeded, Output: "stale"},
		} {
			go func(response sdkdelegation.Response) {
				<-start
				results <- board.Complete(
					context.Background(), id, first.LeaseToken, response)
			}(response)
		}
		close(start)
		for range 2 {
			if err := <-results; err != nil {
				t.Fatalf("stale Complete: %v", err)
			}
		}

		status, err := board.Status(context.Background(), id)
		if err != nil || status.Status != sdkdelegation.StatusRunning {
			t.Fatalf("status after stale completions = %+v, %v", status, err)
		}
		if err := board.Complete(
			context.Background(),
			id,
			current.LeaseToken,
			sdkdelegation.Response{
				Status: sdkdelegation.StatusSucceeded,
				Output: "current",
			},
		); err != nil {
			t.Fatalf("current Complete: %v", err)
		}
		status, err = board.Status(context.Background(), id)
		if err != nil || status.Status != sdkdelegation.StatusSucceeded ||
			status.Output != "current" {
			t.Fatalf("terminal status = %+v, %v", status, err)
		}
	}
}

func TestCapacityAndTerminalEviction(t *testing.T) {
	t.Run("pending capacity", func(t *testing.T) {
		board := newBoard(t, kanban.WithMaxPending(1))
		first := submit(t, board, "worker")
		if _, err := board.Submit(context.Background(), request("worker")); !errdefs.IsRateLimit(err) {
			t.Fatalf("capacity error = %v", err)
		}
		if !board.ClaimCard(first, "worker") {
			t.Fatal("ClaimCard failed")
		}
		if _, err := board.Submit(context.Background(), request("worker")); err != nil {
			t.Fatalf("Submit after claim: %v", err)
		}
	})
	t.Run("max cards spares outstanding", func(t *testing.T) {
		board := newBoard(t, kanban.WithMaxCards(1))
		terminal := submit(t, board, "worker")
		work, _ := board.Claim(context.Background())
		if err := board.Complete(context.Background(), terminal, work.LeaseToken, sdkdelegation.Response{
			Status: sdkdelegation.StatusSucceeded,
		}); err != nil {
			t.Fatal(err)
		}
		pending := submit(t, board, "worker")
		if _, ok := board.Card(terminal); ok {
			t.Fatal("terminal card survived max-card eviction")
		}
		if _, ok := board.Card(pending); !ok {
			t.Fatal("pending card was evicted")
		}
	})
	t.Run("ttl spares outstanding", func(t *testing.T) {
		board := newBoard(t, kanban.WithCardTTL(time.Nanosecond))
		terminal := submit(t, board, "worker")
		work, _ := board.Claim(context.Background())
		_ = board.Complete(context.Background(), terminal, work.LeaseToken, sdkdelegation.Response{
			Status: sdkdelegation.StatusFailed,
			Error:  "failed",
		})
		pending := submit(t, board, "worker")
		time.Sleep(time.Millisecond)
		submit(t, board, "worker")
		if _, ok := board.Card(terminal); ok {
			t.Fatal("expired terminal card survived")
		}
		if _, ok := board.Card(pending); !ok {
			t.Fatal("TTL evicted pending work")
		}
	})
	t.Run("ttl eviction preserves terminal tombstone during retention", func(t *testing.T) {
		board := newBoard(t,
			kanban.WithCardTTL(time.Millisecond),
			kanban.WithIdempotencyRetention(100*time.Millisecond),
		)
		req := request("worker")
		req.Request.IdempotencyKey = "ttl-key"
		first, err := board.Submit(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		work, err := board.Claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := board.Complete(context.Background(), first, work.LeaseToken, sdkdelegation.Response{
			Status: sdkdelegation.StatusSucceeded,
			Output: "done",
		}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(100 * time.Millisecond)
		for {
			if _, ok := board.Card(first); !ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("terminal card was not evicted while board was idle")
			}
			time.Sleep(time.Millisecond)
		}
		replayed, err := board.Submit(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if replayed != first {
			t.Fatalf("replayed id = %q, want %q", replayed, first)
		}
		status, err := board.Status(context.Background(), first)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status != sdkdelegation.StatusSucceeded || status.Output != "done" {
			t.Fatalf("tombstone status = %+v", status)
		}
		different := req
		different.Request.Input = "different"
		if _, err := board.Submit(context.Background(), different); !errdefs.IsConflict(err) {
			t.Fatalf("different request error = %v, want conflict", err)
		}
		if board.Len() != 0 {
			t.Fatalf("replay created work; retained cards = %d", board.Len())
		}
	})
	t.Run("capacity eviction preserves terminal tombstone during retention", func(t *testing.T) {
		board := newBoard(t,
			kanban.WithMaxCards(1),
			kanban.WithIdempotencyRetention(time.Second),
		)
		req := request("worker")
		req.Request.IdempotencyKey = "evicted-key"
		first, err := board.Submit(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		work, err := board.Claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := board.Complete(context.Background(), first, work.LeaseToken, sdkdelegation.Response{
			Status: sdkdelegation.StatusSucceeded,
			Output: "done",
		}); err != nil {
			t.Fatal(err)
		}
		submit(t, board, "other")
		if _, ok := board.Card(first); ok {
			t.Fatal("terminal card survived max-card eviction")
		}
		replayed, err := board.Submit(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if replayed != first {
			t.Fatalf("replayed id = %q, want %q", replayed, first)
		}
		status, err := board.Status(context.Background(), first)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status != sdkdelegation.StatusSucceeded || status.Output != "done" {
			t.Fatalf("tombstone status = %+v", status)
		}
	})
	t.Run("expired tombstone permits a new operation", func(t *testing.T) {
		board := newBoard(t,
			kanban.WithCardTTL(time.Millisecond),
			kanban.WithIdempotencyRetention(5*time.Millisecond),
		)
		req := request("worker")
		req.Request.IdempotencyKey = "expiring-key"
		first, err := board.Submit(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		work, err := board.Claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := board.Complete(context.Background(), first, work.LeaseToken, sdkdelegation.Response{
			Status: sdkdelegation.StatusSucceeded,
		}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(100 * time.Millisecond)
		for {
			_, err = board.Status(context.Background(), first)
			if errdefs.IsNotFound(err) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("expired tombstone remained queryable")
			}
			time.Sleep(time.Millisecond)
		}
		req.Request.Input = "new operation after retention"
		second, err := board.Submit(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if second == first {
			t.Fatalf("expired tombstone replayed id %q", first)
		}
	})
}

func TestIdempotencyRetentionMustBePositive(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a non-positive idempotency retention")
		}
	}()
	_ = kanban.New("invalid", kanban.WithIdempotencyRetention(0))
}

func TestCompleteRejectsInvalidTransitions(t *testing.T) {
	board := newBoard(t)
	if err := board.Complete(
		context.Background(),
		"missing",
		"stale-token",
		sdkdelegation.Response{Status: sdkdelegation.StatusSucceeded},
	); err != nil {
		t.Fatalf("Complete(stale missing) error = %v", err)
	}
	id := submit(t, board, "worker")
	if err := board.Complete(context.Background(), id, "stale-token", sdkdelegation.Response{
		Status: sdkdelegation.StatusSucceeded,
	}); err != nil {
		t.Fatalf("Complete(stale pending) error = %v", err)
	}
	work, err := board.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := board.Complete(context.Background(), id, work.LeaseToken, sdkdelegation.Response{
		Status: sdkdelegation.StatusRunning,
	}); !errdefs.IsValidation(err) {
		t.Fatalf("Complete(nonterminal) error = %v", err)
	}
}
