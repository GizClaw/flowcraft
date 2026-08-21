package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// finalizeErrorStateKey is recorded on the turn result state when the
// engine never published its required run-end event, so the stream
// drain budget was consumed and the turn's sinks were detached without
// seeing the logical run end. Callers can distinguish this contract
// violation from a healthy turn by checking the key.
const finalizeErrorStateKey = "session.finalize_error"

// Turn is one asynchronous execution owned by a Session.
type Turn struct {
	session *Session
	runID   string
	runCtx  context.Context
	cancel  context.CancelFunc
	// release drops the epoch reference acquired at Start/Resume. It is
	// idempotent and invoked exactly when the turn stops using its
	// epoch's dependencies (in finish, or on startTurnLocked failure).
	release func()
	// checkpoints and resume are the turn's epoch durability settings,
	// pinned so afterTurn writes session state to the store the turn
	// started on even across a generation swap.
	checkpoints agent.CheckpointStore
	resume      bool

	resumeFrom *agent.Checkpoint
	resumeCtx  *agent.ResumeContext
	snapshot   *agent.BoardSnapshot
	request    agent.Request

	interrupts chan agent.Interrupt
	done       chan struct{}

	mu                sync.Mutex
	state             TurnState
	result            *agent.Result
	err               error
	interrupt         *agent.Interrupt
	host              agent.Host
	askUserOverride   AskUserFunc
	prompts           map[string]*promptEntry
	attachments       []*queuedSink
	coordinator       *streamCoordinator
	coordinatorDetach func()

	authorityID      string
	authorityAckMode AckMode
	authorityMax     DeliveryCursor
	authoritySink    *queuedSink
	offeredCursor    DeliveryCursor
	deliveredCursor  DeliveryCursor
	ackedCursor      DeliveryCursor
	commitAttempt    int
	tokenByCursor    map[DeliveryCursor]tokenRecord
	ackedPrefix      strings.Builder
	frozen           bool
	frozenCursor     DeliveryCursor
	frozenPrefix     string
}

type tokenRecord struct {
	attempt int
	content string
}

var (
	_ agent.Referee            = (*Turn)(nil)
	_ agent.CommitViewProvider = (*Turn)(nil)
	_ agent.Observer           = (*Turn)(nil)
)

func newTurn(session *Session, runID string, parent context.Context) *Turn {
	runCtx, cancel := context.WithCancel(parent)
	return &Turn{
		session:       session,
		runID:         runID,
		runCtx:        runCtx,
		cancel:        cancel,
		interrupts:    make(chan agent.Interrupt, 1),
		done:          make(chan struct{}),
		state:         TurnStarting,
		prompts:       make(map[string]*promptEntry),
		commitAttempt: 1,
		tokenByCursor: make(map[DeliveryCursor]tokenRecord),
	}
}

// RunID returns the immutable root execution identifier.
func (t *Turn) RunID() string {
	if t == nil {
		return ""
	}
	return t.runID
}

// State returns the current lifecycle state.
func (t *Turn) State() TurnState {
	if t == nil {
		return TurnFailed
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// Cancel immediately stops the turn by cancelling its execution
// context. Unlike [Turn.Interrupt], which the engine only observes at
// its next safe checkpoint, Cancel takes effect as soon as the engine
// notices the context cancellation — typically while it is blocked in
// I/O or an inference call.
//
// The run settles as [agent.StatusCanceled], a non-committed outcome:
// with resume enabled the session parks the run and its checkpoint
// stays resumable via [Session.Resume]. Cancel is idempotent and safe
// to call after the turn has finished.
func (t *Turn) Cancel() {
	if t == nil {
		return
	}
	t.cancel()
}

// Interrupt cooperatively asks the engine to stop. The first cause wins.
func (t *Turn) Interrupt(interrupt agent.Interrupt) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.state.isTerminal() || t.interrupt != nil {
		t.mu.Unlock()
		return nil
	}
	saved := interrupt
	t.freezeLocked()
	t.interrupt = &saved
	t.state = TurnInterrupting
	select {
	case t.interrupts <- interrupt:
	default:
	}
	resolved := t.interruptPendingPromptsLocked(interrupt)
	t.mu.Unlock()
	t.finishPromptActivity(len(resolved))
	for _, r := range resolved {
		t.publishPromptResolved(t.host, r.promptID, r.status)
	}
	return nil
}

// Ack cumulatively acknowledges confirmed deliveries from the authoritative
// explicit sink. It may be called from Sink.OnDelta for the cursor currently
// being offered to that callback.
func (t *Turn) Ack(sinkID string, cursor DeliveryCursor) error {
	if t == nil {
		return errdefs.Validationf("runtime session: nil Turn")
	}
	if cursor == 0 {
		return errdefs.Validationf("runtime session: ACK cursor must be positive")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if sinkID == "" || sinkID != t.authorityID {
		return errdefs.Validationf("runtime session: sink %q is not authoritative", sinkID)
	}
	if t.authorityAckMode != AckExplicit {
		return errdefs.Validationf("runtime session: sink %q does not use explicit ACK", sinkID)
	}
	if cursor <= t.ackedCursor {
		return nil
	}
	if t.frozen {
		return errdefs.Conflictf("runtime session: ACK prefix is frozen at cursor %d", t.frozenCursor)
	}
	if cursor > t.offeredCursor {
		return errdefs.Conflictf(
			"runtime session: ACK cursor %d exceeds offered cursor %d",
			cursor, t.offeredCursor)
	}
	t.advanceAckLocked(cursor)
	return nil
}

// Wait waits for terminal completion without affecting execution.
func (t *Turn) Wait(ctx context.Context) (*agent.Result, error) {
	if t == nil {
		return nil, errdefs.Validationf("runtime session: nil Turn")
	}
	if ctx == nil {
		return nil, errdefs.Validationf("runtime session: Wait context is required")
	}
	select {
	case <-t.done:
		return t.terminalResult()
	default:
	}
	select {
	case <-t.done:
		return t.terminalResult()
	case <-ctx.Done():
		return nil, errdefs.FromContext(ctx.Err())
	}
}

func (t *Turn) terminalResult() (*agent.Result, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result, t.err
}

func (t *Turn) execute(instance *agent.Agent, request agent.Request) {
	options := []agent.ExecuteOption{agent.WithHost(t.host), agent.WithObserver(t)}
	if t.resumeFrom != nil {
		options = append(options, agent.WithResumeFrom(t.resumeFrom))
	}
	if t.snapshot != nil {
		options = append(options, agent.WithPreparer(sessionHistoryPreparer(t.snapshot)))
	}
	t.mu.Lock()
	hasAuthority := t.authorityID != ""
	t.mu.Unlock()
	if hasAuthority {
		options = append(options,
			agent.WithReferee(t),
			agent.WithCommitViewProvider(t),
		)
	}
	execCtx := t.runCtx
	if t.resumeCtx != nil {
		execCtx = agent.WithResumeContext(t.runCtx, *t.resumeCtx)
	}
	t.request = request
	result, err := agent.Execute(execCtx, *instance, nil, request, options...)
	t.finish(result, err)
}

// sessionHistoryPreparer restores the session's last committed board
// (conversation history) for a fresh turn, then merges the default
// seed's new message and inputs on top. It is only used for fresh
// starts; Resume passes the checkpoint board instead. Earlier links in
// the Preparer chain run before this one, so their non-main-channel
// output (retrieval, tool state, …) is overlaid on the restored board
// rather than lost.
func sessionHistoryPreparer(history *agent.BoardSnapshot) agent.Preparer {
	return agent.PreparerFunc(func(
		_ context.Context,
		_ agent.Identity,
		_ *agent.Request,
		prev *agent.Board,
	) (*agent.Board, error) {
		board := agent.RestoreBoard(history)
		for _, msg := range prev.Channel(agent.MainChannel) {
			board.AppendChannelMessage(agent.MainChannel, msg)
		}
		for key, msgs := range prev.ChannelsCopy() {
			if key == agent.MainChannel {
				continue
			}
			board.SetChannel(key, msgs)
		}
		for key, value := range prev.Vars() {
			board.SetVar(key, value)
		}
		return board, nil
	})
}

func (t *Turn) finish(result *agent.Result, err error) {
	defer t.cancel()

	t.mu.Lock()
	if t.state.isTerminal() {
		t.mu.Unlock()
		return
	}
	t.result = result
	t.err = err
	t.state = terminalState(result, err)
	resolved := t.closePendingPromptsLocked()
	attachments := append([]*queuedSink(nil), t.attachments...)
	coordinator := t.coordinator
	detachCoordinator := t.coordinatorDetach
	t.mu.Unlock()

	t.finishPromptActivity(len(resolved))
	for _, r := range resolved {
		t.publishPromptResolved(t.host, r.promptID, r.status)
	}
	var runEndErr *agent.RunEndPublishError
	runEndFailed := result != nil && errors.As(result.Err, &runEndErr)
	var finalizeErr error
	if result != nil && !runEndFailed && coordinator != nil {
		finalizeCtx, cancel := context.WithTimeout(
			context.WithoutCancel(t.runCtx),
			defaultAttemptDrainTimeout,
		)
		finalizeErr = coordinator.finalize(finalizeCtx, result, err)
		cancel()
		if finalizeErr != nil {
			t.recordFinalizeTimeout(result, finalizeErr)
		}
	}
	for _, attachment := range attachments {
		if result == nil || runEndFailed || finalizeErr != nil {
			detachErr := err
			if runEndFailed {
				detachErr = runEndErr
			} else if finalizeErr != nil {
				detachErr = finalizeErr
			}
			attachment.detach(detachErr)
		} else {
			attachment.wait()
		}
	}
	if detachCoordinator != nil {
		detachCoordinator()
	}
	if t.session != nil {
		t.session.afterTurn(t, result)
	}
	if t.session != nil {
		t.session.turnFinished(t, result, err)
	}
	if t.release != nil {
		t.release()
	}
	close(t.done)
}

// recordFinalizeTimeout makes a missing run-end diagnosable: the engine
// did not publish its required terminal event within the drain budget,
// so the turn result carries the reason and telemetry emits a warning.
// The drain timeout itself is not returned from Wait because the turn
// outcome (result/status) already settled; it is recorded instead.
func (t *Turn) recordFinalizeTimeout(result *agent.Result, finalizeErr error) {
	if result.State == nil {
		result.State = make(map[string]any)
	}
	result.State[finalizeErrorStateKey] = finalizeErr.Error()
	telemetry.WarnErr(
		context.WithoutCancel(t.runCtx),
		"runtime session: engine did not publish run-end; turn stream drain timed out",
		finalizeErr,
		otellog.String(telemetry.AttrRunID, t.runID),
	)
}

func (t *Turn) configureAuthority(spec SinkSpec, queueSize int, sink *queuedSink) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.authorityID = spec.ID
	t.authorityAckMode = spec.AckMode
	limit := spec.MaxUnacked
	if limit == 0 {
		limit = queueSize
	}
	t.authorityMax = DeliveryCursor(limit)
	t.authoritySink = sink
}

func (t *Turn) recordConfirmed(attempt int, cursor DeliveryCursor, delta agent.StreamDeltaPayload) {
	t.mu.Lock()
	if t.authorityID != "" &&
		!t.frozen &&
		attempt == t.commitAttempt &&
		delta.Type == agent.StreamDeltaPart {
		if text, ok := delta.Part.(message.TextPart); ok {
			t.tokenByCursor[cursor] = tokenRecord{attempt: attempt, content: text.Text}
		}
	}
	t.mu.Unlock()
}

func (t *Turn) sinkOffered(sinkID string, cursor DeliveryCursor) {
	t.mu.Lock()
	if sinkID == t.authorityID && cursor > t.offeredCursor {
		t.offeredCursor = cursor
	}
	t.mu.Unlock()
}

func (t *Turn) sinkDelivered(sinkID string, cursor DeliveryCursor) {
	var detach *queuedSink
	t.mu.Lock()
	if sinkID == t.authorityID {
		if cursor > t.offeredCursor {
			t.offeredCursor = cursor
		}
		if cursor > t.deliveredCursor {
			t.deliveredCursor = cursor
		}
		if t.authorityAckMode == AckOnDelivery && !t.frozen {
			t.advanceAckLocked(t.deliveredCursor)
		}
		if t.authorityMax > 0 && t.deliveredCursor-t.ackedCursor > t.authorityMax {
			detach = t.authoritySink
		}
	}
	t.mu.Unlock()
	if detach != nil {
		detach.detach(errdefs.BudgetExceededf(
			"runtime session: authoritative sink exceeded MaxUnacked"))
	}
}

func (t *Turn) sinkDetached(sinkID string, _ error) {
	t.mu.Lock()
	if sinkID == t.authorityID {
		t.freezeLocked()
	}
	t.mu.Unlock()
}

func (t *Turn) advanceAckLocked(cursor DeliveryCursor) {
	if cursor <= t.ackedCursor {
		return
	}
	for current := t.ackedCursor + 1; current <= cursor; current++ {
		record, ok := t.tokenByCursor[current]
		if ok && record.attempt == t.commitAttempt {
			t.ackedPrefix.WriteString(record.content)
		}
		delete(t.tokenByCursor, current)
	}
	t.ackedCursor = cursor
}

func (t *Turn) freezeLocked() {
	if t.frozen {
		return
	}
	t.frozen = true
	t.frozenCursor = t.ackedCursor
	t.frozenPrefix = t.ackedPrefix.String()
	clear(t.tokenByCursor)
}

// OnRunStart implements agent.Observer.
func (t *Turn) OnRunStart(context.Context, agent.Identity, *agent.Request) {}

// OnInterrupt implements agent.Observer. Turn.finish owns logical completion.
func (t *Turn) OnInterrupt(context.Context, agent.Identity, agent.Interrupt) {}

// OnRunRevise moves commit authority to the next attempt while preserving
// turn-global delivery and acknowledgement cursors.
func (t *Turn) OnRunRevise(
	_ context.Context,
	_ agent.Identity,
	_ *agent.Result,
	nextAttempt int,
) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commitAttempt = nextAttempt
	t.ackedCursor = t.deliveredCursor
	t.ackedPrefix.Reset()
	t.frozen = false
	t.frozenCursor = 0
	t.frozenPrefix = ""
	for cursor, record := range t.tokenByCursor {
		if record.attempt != nextAttempt {
			delete(t.tokenByCursor, cursor)
		}
	}
}

// OnRunEnd implements agent.Observer. Turn.finish owns logical completion.
func (t *Turn) OnRunEnd(context.Context, agent.Identity, *agent.Result) {}

// After implements the turn-scoped interruption referee.
func (t *Turn) After(_ context.Context, _ agent.Identity, _ *agent.Request, res *agent.Result) (agent.Decision, error) {
	if res == nil || res.Status != agent.StatusInterrupted {
		return agent.Decision{}, nil
	}
	t.mu.Lock()
	t.freezeLocked()
	prefix := t.frozenPrefix
	t.mu.Unlock()
	if prefix == "" {
		return agent.Decision{}, nil
	}
	return agent.Decision{AcceptOutput: true, Reason: "acknowledged stream prefix"}, nil
}

// CommitView implements the turn-scoped Committer projection.
func (t *Turn) CommitView(_ context.Context, _ agent.Identity, _ *agent.Request, res *agent.Result) (agent.CommitView, error) {
	if res == nil || res.LastBoard == nil {
		return agent.CommitView{}, errdefs.Validationf("runtime session: commit view requires a result board")
	}
	if res.Status != agent.StatusInterrupted {
		return agent.CommitView{LastBoard: res.LastBoard}, nil
	}
	t.mu.Lock()
	t.freezeLocked()
	prefix := t.frozenPrefix
	t.mu.Unlock()
	output := trailingAssistantText(res.Messages)
	if !utf8.ValidString(prefix) || !utf8.ValidString(output) {
		return agent.CommitView{}, errdefs.Validationf("runtime session: assistant output is not valid UTF-8")
	}
	if !strings.HasPrefix(output, prefix) {
		return agent.CommitView{}, errdefs.Conflictf(
			"runtime session: acknowledged prefix does not match assistant output")
	}
	board := res.LastBoard.Clone()
	messages := board.Channel(agent.MainChannel)
	for len(messages) > 0 && messages[len(messages)-1].Role == message.RoleAssistant {
		messages = messages[:len(messages)-1]
	}
	if prefix != "" {
		messages = append(messages, message.NewTextMessage(message.RoleAssistant, prefix))
	}
	board.SetChannel(agent.MainChannel, messages)
	return agent.CommitView{LastBoard: board}, nil
}

func trailingAssistantText(messages []message.Message) string {
	start := len(messages)
	for start > 0 && messages[start-1].Role == message.RoleAssistant {
		start--
	}
	var builder strings.Builder
	for _, message := range messages[start:] {
		builder.WriteString(message.Content.Text())
	}
	return builder.String()
}

func terminalState(result *agent.Result, err error) TurnState {
	if result != nil {
		switch result.Status {
		case agent.StatusCompleted:
			return TurnCompleted
		case agent.StatusInterrupted:
			return TurnInterrupted
		case agent.StatusCanceled:
			return TurnCanceled
		case agent.StatusAborted:
			return TurnAborted
		default:
			return TurnFailed
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return TurnCanceled
		}
	}
	return TurnFailed
}

func (t *Turn) shutdown() {
	if err := t.Interrupt(agent.Interrupt{Cause: agent.CauseHostShutdown}); err != nil {
		telemetry.WarnErr(context.WithoutCancel(t.runCtx),
			"runtime session: interrupt turn on host shutdown failed", err,
			otellog.String(telemetry.AttrRunID, t.runID))
	}
}
