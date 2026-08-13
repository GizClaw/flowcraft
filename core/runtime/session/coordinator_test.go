package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
)

type capturedDelivery struct {
	env   event.Envelope
	delta agent.StreamDeltaPayload
}

type captureSink struct {
	mu    sync.Mutex
	items []capturedDelivery
}

func (s *captureSink) OnDelta(_ context.Context, env event.Envelope, delta agent.StreamDeltaPayload) error {
	s.mu.Lock()
	s.items = append(s.items, capturedDelivery{env: env, delta: delta})
	s.mu.Unlock()
	return nil
}

func (s *captureSink) snapshot() []capturedDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedDelivery(nil), s.items...)
}

func TestStreamCoordinatorRawAndConfirmedSpeculativeFlow(t *testing.T) {
	if HeaderDeliveryCursor != "session_delivery_cursor" {
		t.Fatalf("HeaderDeliveryCursor = %q", HeaderDeliveryCursor)
	}
	turn := newTurn(nil, "run-1", context.Background())
	rawCapture := &captureSink{}
	confirmedCapture := &captureSink{}
	raw := newQueuedSink(nil, "run-1", SinkSpec{ID: "raw", Sink: rawCapture}, 8)
	confirmed := newQueuedSink(nil, "run-1", SinkSpec{
		ID: "confirmed", Sink: confirmedCapture, Visibility: VisibilityConfirmed,
	}, 8)
	raw.start()
	confirmed.start()
	coordinator := newStreamCoordinator(turn, []*queuedSink{raw}, []*queuedSink{confirmed}, 8, 4096)

	speculative := agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "hello"},
		Speculative: true, ForkID: "fork", BranchID: "branch",
	}
	accept := agent.StreamDeltaPayload{
		Type: agent.StreamDeltaParallelBranchAccept, ForkID: "fork", BranchID: "branch",
	}
	_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, speculative), speculative)
	_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, accept), accept)
	end := event.MustEnvelope(context.Background(), agent.SubjectRunEnd("run-1"), nil)
	_ = coordinator.OnDelta(context.Background(), end, agent.StreamDeltaPayload{})
	finalizeCoordinator(coordinator, 1)
	raw.wait()
	confirmed.wait()

	rawItems := rawCapture.snapshot()
	if len(rawItems) != 3 || tokenText(rawItems[0].delta) != "hello" {
		t.Fatalf("raw deliveries = %#v", rawItems)
	}
	confirmedItems := confirmedCapture.snapshot()
	if len(confirmedItems) != 3 ||
		confirmedItems[0].delta.Type != agent.StreamDeltaParallelBranchAccept ||
		tokenText(confirmedItems[1].delta) != "hello" {
		t.Fatalf("confirmed deliveries = %#v", confirmedItems)
	}
	for index, item := range confirmedItems {
		cursor, err := DeliveryCursorFromEnvelope(item.env)
		if err != nil || cursor != DeliveryCursor(index+1) {
			t.Fatalf("delivery %d cursor = %d, %v", index, cursor, err)
		}
	}
	if _, err := DeliveryCursorFromEnvelope(rawItems[0].env); !errdefs.IsValidation(err) {
		t.Fatalf("raw cursor error = %v", err)
	}
}

func TestStreamCoordinatorCancelDropsAndConflictDetachesConfirmed(t *testing.T) {
	turn := newTurn(nil, "run-2", context.Background())
	detached := make(chan error, 1)
	confirmed := newQueuedSink(nil, "run-2", SinkSpec{
		ID: "confirmed", Sink: &captureSink{}, Visibility: VisibilityConfirmed,
		OnDetach: func(err error) { detached <- err },
	}, 8)
	confirmed.start()
	coordinator := newStreamCoordinator(turn, nil, []*queuedSink{confirmed}, 8, 4096)
	data := agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "discard"},
		Speculative: true, ForkID: "fork", BranchID: "branch",
	}
	cancel := agent.StreamDeltaPayload{
		Type: agent.StreamDeltaParallelBranchCancel, ForkID: "fork", BranchID: "branch",
	}
	accept := agent.StreamDeltaPayload{
		Type: agent.StreamDeltaParallelBranchAccept, ForkID: "fork", BranchID: "branch",
	}
	_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, data), data)
	_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, cancel), cancel)
	_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, cancel), cancel)
	_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, accept), accept)
	if err := <-detached; !errdefs.IsConflict(err) {
		t.Fatalf("detach error = %v", err)
	}
}

func TestStreamCoordinatorMultipleForksPreserveIndependentOrder(t *testing.T) {
	turn := newTurn(nil, "run-multi", context.Background())
	capture := &captureSink{}
	confirmed := newQueuedSink(nil, "run-multi", SinkSpec{
		ID: "confirmed", Sink: capture, Visibility: VisibilityConfirmed,
	}, 16)
	confirmed.start()
	coordinator := newStreamCoordinator(turn, nil, []*queuedSink{confirmed}, 8, 4096)
	dataA := agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "a"}, Speculative: true,
		ForkID: "fork-a", BranchID: "branch",
	}
	dataB := agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "b"}, Speculative: true,
		ForkID: "fork-b", BranchID: "branch",
	}
	for _, delta := range []agent.StreamDeltaPayload{
		dataA,
		dataB,
		{Type: agent.StreamDeltaParallelBranchAccept, ForkID: "fork-b", BranchID: "branch"},
		{Type: agent.StreamDeltaParallelBranchCancel, ForkID: "fork-a", BranchID: "branch"},
	} {
		_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, delta), delta)
	}
	end := event.MustEnvelope(context.Background(), agent.SubjectRunEnd("run-multi"), nil)
	_ = coordinator.OnDelta(context.Background(), end, agent.StreamDeltaPayload{})
	finalizeCoordinator(coordinator, 1)
	confirmed.wait()
	items := capture.snapshot()
	if len(items) != 4 ||
		items[0].delta.Type != agent.StreamDeltaParallelBranchAccept ||
		tokenText(items[1].delta) != "b" ||
		items[2].delta.Type != agent.StreamDeltaParallelBranchCancel {
		t.Fatalf("deliveries = %#v", items)
	}
}

func TestStreamCoordinatorRunEndWithUnterminatedBranchDetachesConfirmed(t *testing.T) {
	turn := newTurn(nil, "run-open-branch", context.Background())
	rawCapture := &captureSink{}
	raw := newQueuedSink(nil, "run-open-branch", SinkSpec{ID: "raw", Sink: rawCapture}, 4)
	confirmedCapture := &captureSink{}
	detached := make(chan error, 1)
	confirmed := newQueuedSink(nil, "run-open-branch", SinkSpec{
		ID:         "confirmed",
		Sink:       confirmedCapture,
		Visibility: VisibilityConfirmed,
		OnDetach:   func(err error) { detached <- err },
	}, 4)
	raw.start()
	confirmed.start()
	coordinator := newStreamCoordinator(
		turn, []*queuedSink{raw}, []*queuedSink{confirmed}, 4, 4096)

	data := agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "pending"}, Speculative: true,
		ForkID: "fork", BranchID: "branch",
	}
	_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, data), data)
	end := event.MustEnvelope(context.Background(), agent.SubjectRunEnd("run-open-branch"), nil)
	_ = coordinator.OnDelta(context.Background(), end, agent.StreamDeltaPayload{})
	finalizeCoordinator(coordinator, 1)

	select {
	case err := <-detached:
		if !errdefs.IsConflict(err) {
			t.Fatalf("detach error = %v, want conflict", err)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed sink was not detached")
	}
	raw.wait()
	if got := rawCapture.snapshot(); len(got) != 2 {
		t.Fatalf("raw deliveries = %d, want speculative data and run end", len(got))
	}
	if got := confirmedCapture.snapshot(); len(got) != 0 {
		t.Fatalf("confirmed deliveries = %#v, want no normal run end", got)
	}
}

func TestStreamCoordinatorLimitDetachesConfirmedButRawContinues(t *testing.T) {
	turn := newTurn(nil, "run-limit", context.Background())
	rawCapture := &captureSink{}
	raw := newQueuedSink(nil, "run-limit", SinkSpec{ID: "raw", Sink: rawCapture}, 8)
	detached := make(chan error, 1)
	confirmed := newQueuedSink(nil, "run-limit", SinkSpec{
		ID: "confirmed", Sink: &captureSink{}, Visibility: VisibilityConfirmed,
		OnDetach: func(err error) { detached <- err },
	}, 8)
	raw.start()
	confirmed.start()
	coordinator := newStreamCoordinator(
		turn, []*queuedSink{raw}, []*queuedSink{confirmed}, 1, 4096)
	for _, content := range []string{"one", "two"} {
		delta := agent.StreamDeltaPayload{
			Type: agent.StreamDeltaPart, Part: message.TextPart{Text: content}, Speculative: true,
			ForkID: "fork", BranchID: "branch",
		}
		_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, delta), delta)
	}
	if err := <-detached; !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("detach error = %v", err)
	}
	ordinary := agent.StreamDeltaPayload{Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "raw-only"}}
	_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, ordinary), ordinary)
	end := event.MustEnvelope(context.Background(), agent.SubjectRunEnd("run-limit"), nil)
	_ = coordinator.OnDelta(context.Background(), end, agent.StreamDeltaPayload{})
	finalizeCoordinator(coordinator, 1)
	raw.wait()
	items := rawCapture.snapshot()
	if len(items) != 4 || tokenText(items[2].delta) != "raw-only" {
		t.Fatalf("raw deliveries = %#v", items)
	}
}

func TestStreamCoordinatorBoundsTerminalBranchStates(t *testing.T) {
	turn := newTurn(nil, "run-branches", context.Background())
	detached := make(chan error, 1)
	confirmed := newQueuedSink(nil, "run-branches", SinkSpec{
		ID: "confirmed", Sink: &captureSink{}, Visibility: VisibilityConfirmed,
		OnDetach: func(err error) { detached <- err },
	}, 8)
	confirmed.start()
	coordinator := newStreamCoordinator(turn, nil, []*queuedSink{confirmed}, 2, 4096)
	for index := range 3 {
		delta := agent.StreamDeltaPayload{
			Type:   agent.StreamDeltaParallelBranchAccept,
			ForkID: "fork", BranchID: string(rune('a' + index)),
		}
		_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, delta), delta)
	}
	if err := <-detached; !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("detach error = %v", err)
	}
}

func TestStreamCoordinatorMalformedIdentityDetachesConfirmedRawContinues(t *testing.T) {
	for _, malformed := range []agent.StreamDeltaPayload{
		{Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "bad"}, Speculative: true},
		{Type: agent.StreamDeltaParallelBranchAccept, ForkID: "fork"},
	} {
		turn := newTurn(nil, "run-malformed", context.Background())
		rawCapture := &captureSink{}
		raw := newQueuedSink(nil, "run-malformed", SinkSpec{ID: "raw", Sink: rawCapture}, 4)
		detached := make(chan error, 1)
		confirmed := newQueuedSink(nil, "run-malformed", SinkSpec{
			ID: "confirmed", Sink: &captureSink{}, Visibility: VisibilityConfirmed,
			OnDetach: func(err error) { detached <- err },
		}, 4)
		raw.start()
		confirmed.start()
		coordinator := newStreamCoordinator(
			turn, []*queuedSink{raw}, []*queuedSink{confirmed}, 4, 4096)
		_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, malformed), malformed)
		if err := <-detached; !errdefs.IsValidation(err) {
			t.Fatalf("detach error = %v", err)
		}
		end := event.MustEnvelope(context.Background(), agent.SubjectRunEnd("run-malformed"), nil)
		_ = coordinator.OnDelta(context.Background(), end, agent.StreamDeltaPayload{})
		finalizeCoordinator(coordinator, 1)
		raw.wait()
		if got := rawCapture.snapshot(); len(got) != 2 {
			t.Fatalf("raw deliveries = %#v", got)
		}
	}
}

func TestTurnAckFreezeAndCommitView(t *testing.T) {
	turn := newTurn(nil, "run-3", context.Background())
	turn.configureAuthority(SinkSpec{
		ID: "authority", Visibility: VisibilityConfirmed,
		Authority: AuthorityAuthoritative, AckMode: AckExplicit,
	}, 8, nil)
	turn.recordConfirmed(1, 1, agent.StreamDeltaPayload{Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "hello"}})
	turn.recordConfirmed(1, 2, agent.StreamDeltaPayload{Type: agent.StreamDeltaPart, Part: message.TextPart{Text: " world"}})
	turn.sinkDelivered("authority", 2)
	if err := turn.Ack("authority", 1); err != nil {
		t.Fatal(err)
	}
	if err := turn.Interrupt(agent.Interrupt{Cause: agent.CauseUserInput}); err != nil {
		t.Fatal(err)
	}
	if err := turn.Ack("authority", 2); !errdefs.IsConflict(err) {
		t.Fatalf("post-freeze ACK error = %v", err)
	}

	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleAssistant, "hello world"))
	result := &agent.Result{
		Status: agent.StatusInterrupted, LastBoard: board,
		Messages: []message.Message{
			message.NewTextMessage(message.RoleAssistant, "hello world"),
		},
	}
	decision, err := turn.After(context.Background(), agent.Identity{}, nil, result)
	if err != nil || !decision.AcceptOutput {
		t.Fatalf("decision = %#v, %v", decision, err)
	}
	view, err := turn.CommitView(context.Background(), agent.Identity{}, nil, result)
	if err != nil {
		t.Fatal(err)
	}
	got := view.LastBoard.Channel(agent.MainChannel)
	if len(got) != 1 || got[0].Content.Text() != "hello" {
		t.Fatalf("projected messages = %#v", got)
	}
	if result.Text() != "hello world" {
		t.Fatalf("original result changed: %q", result.Text())
	}
}

func TestTurnAckValidationAndAutomaticMode(t *testing.T) {
	explicit := newTurn(nil, "run-explicit", context.Background())
	explicit.configureAuthority(SinkSpec{
		ID: "authority", Visibility: VisibilityConfirmed,
		Authority: AuthorityAuthoritative, AckMode: AckExplicit,
	}, 2, nil)
	explicit.sinkDelivered("authority", 1)
	if err := explicit.Ack("observer", 1); !errdefs.IsValidation(err) {
		t.Fatalf("observer ACK error = %v", err)
	}
	if err := explicit.Ack("authority", 2); !errdefs.IsConflict(err) {
		t.Fatalf("future ACK error = %v", err)
	}
	if err := explicit.Ack("authority", 0); !errdefs.IsValidation(err) {
		t.Fatalf("zero ACK error = %v", err)
	}
	if err := explicit.Ack("authority", 1); err != nil {
		t.Fatal(err)
	}
	if err := explicit.Ack("authority", 1); err != nil {
		t.Fatalf("duplicate ACK error = %v", err)
	}

	automatic := newTurn(nil, "run-auto", context.Background())
	automatic.configureAuthority(SinkSpec{
		ID: "authority", Visibility: VisibilityConfirmed,
		Authority: AuthorityAuthoritative,
	}, 2, nil)
	automatic.sinkDelivered("authority", 1)
	if err := automatic.Ack("authority", 1); !errdefs.IsValidation(err) {
		t.Fatalf("automatic ACK error = %v", err)
	}
	automatic.mu.Lock()
	acked := automatic.ackedCursor
	automatic.mu.Unlock()
	if acked != 1 {
		t.Fatalf("automatic acknowledged cursor = %d", acked)
	}
}

func TestTurnAutomaticAckReleasesTokenLedger(t *testing.T) {
	turn := newTurn(nil, "run-auto-long", context.Background())
	turn.configureAuthority(SinkSpec{
		ID: "authority", Visibility: VisibilityConfirmed,
		Authority: AuthorityAuthoritative,
	}, 8, nil)
	for cursor := DeliveryCursor(1); cursor <= 1000; cursor++ {
		turn.recordConfirmed(1, cursor, agent.StreamDeltaPayload{
			Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "x"},
		})
		turn.sinkDelivered("authority", cursor)
	}
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if len(turn.tokenByCursor) != 0 {
		t.Fatalf("retained acknowledged tokens = %d", len(turn.tokenByCursor))
	}
	if turn.ackedPrefix.Len() != 1000 {
		t.Fatalf("acknowledged prefix length = %d", turn.ackedPrefix.Len())
	}
}

func TestTurnWithoutAuthorityDoesNotRetainConfirmedTokens(t *testing.T) {
	turn := newTurn(nil, "run-observer-only", context.Background())
	for cursor := DeliveryCursor(1); cursor <= 1000; cursor++ {
		turn.recordConfirmed(1, cursor, agent.StreamDeltaPayload{
			Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "x"},
		})
	}
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if len(turn.tokenByCursor) != 0 {
		t.Fatalf("retained observer-only tokens = %d", len(turn.tokenByCursor))
	}
}

func TestStreamCoordinatorFinalizeTimesOutWithoutAttemptBoundary(t *testing.T) {
	turn := newTurn(nil, "run-missing-attempt-end", context.Background())
	coordinator := newStreamCoordinator(turn, nil, nil, 8, 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := coordinator.finalize(ctx, &agent.Result{
		Status:   agent.StatusCompleted,
		Attempts: 1,
	}, nil)
	if !errdefs.IsTimeout(err) {
		t.Fatalf("finalize error = %v, want Timeout", err)
	}
}

func TestTurnCommitViewCompletedAndEmptyInterruptedPrefix(t *testing.T) {
	turn := newTurn(nil, "run-view", context.Background())
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleAssistant, "unacknowledged"))
	completed := &agent.Result{
		Status: agent.StatusCompleted, LastBoard: board,
		Messages: []message.Message{
			message.NewTextMessage(message.RoleAssistant, "unacknowledged"),
		},
	}
	view, err := turn.CommitView(context.Background(), agent.Identity{}, nil, completed)
	if err != nil || view.LastBoard != board {
		t.Fatalf("completed view = %#v, %v", view, err)
	}

	interrupted := *completed
	interrupted.Status = agent.StatusInterrupted
	view, err = turn.CommitView(context.Background(), agent.Identity{}, nil, &interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if got := view.LastBoard.Channel(agent.MainChannel); len(got) != 0 {
		t.Fatalf("empty-prefix projected messages = %#v", got)
	}
}

func TestTurnCommitViewValidatesAllTrailingAssistantMessages(t *testing.T) {
	turn := newTurn(nil, "run-messages", context.Background())
	turn.configureAuthority(SinkSpec{
		ID: "authority", Visibility: VisibilityConfirmed,
		Authority: AuthorityAuthoritative, AckMode: AckExplicit,
	}, 8, nil)
	turn.recordConfirmed(1, 1, agent.StreamDeltaPayload{Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "hello world"}})
	turn.sinkDelivered("authority", 1)
	if err := turn.Ack("authority", 1); err != nil {
		t.Fatal(err)
	}
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleAssistant, "hello "))
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleAssistant, "world!"))
	result := &agent.Result{
		Status: agent.StatusInterrupted, LastBoard: board,
		Messages: []message.Message{
			message.NewTextMessage(message.RoleAssistant, "hello "),
			message.NewTextMessage(message.RoleAssistant, "world!"),
		},
	}
	view, err := turn.CommitView(context.Background(), agent.Identity{}, nil, result)
	if err != nil {
		t.Fatal(err)
	}
	got := view.LastBoard.Channel(agent.MainChannel)
	if len(got) != 1 || got[0].Content.Text() != "hello world" {
		t.Fatalf("projected messages = %#v", got)
	}
}

func TestTurnReviseKeepsOnlyCurrentAttemptAcknowledgedPrefix(t *testing.T) {
	turn := newTurn(nil, "run-revise-ledger", context.Background())
	turn.configureAuthority(SinkSpec{
		ID: "authority", Visibility: VisibilityConfirmed,
		Authority: AuthorityAuthoritative, AckMode: AckExplicit,
	}, 8, nil)

	turn.recordConfirmed(1, 1, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "old"},
	})
	turn.sinkDelivered("authority", 1)
	if err := turn.Ack("authority", 1); err != nil {
		t.Fatal(err)
	}
	turn.OnRunRevise(context.Background(), agent.Identity{}, nil, 2)

	// A delayed old-attempt record and ACK still advance the global
	// cursor, but cannot enter attempt 2's committable prefix.
	turn.recordConfirmed(1, 2, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "-late-old"},
	})
	turn.recordConfirmed(2, 3, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "new"},
	})
	turn.sinkDelivered("authority", 3)
	if err := turn.Ack("authority", 3); err != nil {
		t.Fatal(err)
	}

	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.commitAttempt != 2 {
		t.Fatalf("commit attempt = %d, want 2", turn.commitAttempt)
	}
	if turn.ackedCursor != 3 || turn.deliveredCursor != 3 {
		t.Fatalf("cursors = acked %d delivered %d", turn.ackedCursor, turn.deliveredCursor)
	}
	if got := turn.ackedPrefix.String(); got != "new" {
		t.Fatalf("attempt 2 prefix = %q, want new", got)
	}
}

func TestTurnReviseIgnoresLateAutomaticAckFromOldAttempt(t *testing.T) {
	turn := newTurn(nil, "run-revise-auto", context.Background())
	turn.configureAuthority(SinkSpec{
		ID: "authority", Visibility: VisibilityConfirmed,
		Authority: AuthorityAuthoritative,
	}, 8, nil)
	turn.recordConfirmed(1, 1, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "old"},
	})
	turn.OnRunRevise(context.Background(), agent.Identity{}, nil, 2)

	turn.sinkDelivered("authority", 1)
	turn.recordConfirmed(2, 2, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "new"},
	})
	turn.sinkDelivered("authority", 2)

	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.ackedCursor != 2 || turn.deliveredCursor != 2 {
		t.Fatalf("cursors = acked %d delivered %d", turn.ackedCursor, turn.deliveredCursor)
	}
	if got := turn.ackedPrefix.String(); got != "new" {
		t.Fatalf("attempt 2 automatic prefix = %q, want new", got)
	}
}

func TestTurnReviseDropsOldAttemptUnackedBudget(t *testing.T) {
	turn := newTurn(nil, "run-revise-budget", context.Background())
	turn.configureAuthority(SinkSpec{
		ID: "authority", Visibility: VisibilityConfirmed,
		Authority: AuthorityAuthoritative, AckMode: AckExplicit,
		MaxUnacked: 1,
	}, 1, nil)
	turn.recordConfirmed(1, 1, agent.StreamDeltaPayload{
		Type: agent.StreamDeltaPart, Part: message.TextPart{Text: "old"},
	})
	turn.sinkDelivered("authority", 1)
	turn.OnRunRevise(context.Background(), agent.Identity{}, nil, 2)

	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.ackedCursor != 1 || turn.deliveredCursor != 1 {
		t.Fatalf("revised cursors = acked %d delivered %d, want 1/1",
			turn.ackedCursor, turn.deliveredCursor)
	}
	if len(turn.tokenByCursor) != 0 {
		t.Fatalf("retained old-attempt records = %d", len(turn.tokenByCursor))
	}
}

func TestStreamCoordinatorAttemptBoundariesEmitOneLogicalRunEnd(t *testing.T) {
	turn := newTurn(nil, "run-two-attempts", context.Background())
	rawCapture, confirmedCapture := &captureSink{}, &captureSink{}
	raw := newQueuedSink(nil, turn.runID, SinkSpec{ID: "raw", Sink: rawCapture}, 8)
	confirmed := newQueuedSink(nil, turn.runID, SinkSpec{
		ID: "confirmed", Sink: confirmedCapture, Visibility: VisibilityConfirmed,
	}, 8)
	raw.start()
	confirmed.start()
	coordinator := newStreamCoordinator(
		turn, []*queuedSink{raw}, []*queuedSink{confirmed}, 8, 4096)

	for attempt := 1; attempt <= 2; attempt++ {
		delta := agent.StreamDeltaPayload{
			Type: agent.StreamDeltaPart, Part: message.TextPart{Text: string(rune('0' + attempt))},
		}
		_ = coordinator.OnDelta(context.Background(), envelopeForDelta(t, delta), delta)
		end := event.MustEnvelope(
			context.Background(), agent.SubjectRunEnd(turn.runID), nil)
		_ = coordinator.OnDelta(context.Background(), end, agent.StreamDeltaPayload{})
	}
	finalizeCoordinator(coordinator, 2)
	raw.wait()
	confirmed.wait()

	for name, items := range map[string][]capturedDelivery{
		"raw": rawCapture.snapshot(), "confirmed": confirmedCapture.snapshot(),
	} {
		if len(items) != 3 {
			t.Fatalf("%s deliveries = %#v", name, items)
		}
		runEnds := 0
		for _, item := range items {
			if item.env.Subject == agent.SubjectRunEnd(turn.runID) {
				runEnds++
			}
		}
		if runEnds != 1 {
			t.Fatalf("%s logical run ends = %d, want 1", name, runEnds)
		}
	}
}

func envelopeForDelta(t *testing.T, delta agent.StreamDeltaPayload) event.Envelope {
	t.Helper()
	return event.MustEnvelope(
		context.Background(),
		agent.SubjectStreamDelta("run", "agent"),
		delta,
	)
}

func finalizeCoordinator(coordinator *streamCoordinator, attempts int) {
	_ = coordinator.finalize(context.Background(), &agent.Result{
		Status: agent.StatusCompleted, Attempts: attempts,
	}, nil)
}
