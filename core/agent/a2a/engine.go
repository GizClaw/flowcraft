package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	// engineKind is the stable telemetry token for this engine, matching
	// the registry key used by core/agent/a2a/config.
	engineKind = "a2a"

	// runEndPublishTimeout bounds the best-effort terminal event publish
	// after the run's context has ended.
	runEndPublishTimeout = 5 * time.Second

	// cancelTimeout bounds the tasks/cancel call made on cooperative
	// interrupts.
	cancelTimeout = 5 * time.Second
)

// Board variables this engine writes (under the "a2a." prefix, which is
// outside the "__" reserved namespace but clearly engine-owned).
const (
	// varTaskID carries the remote A2A task id of the current run.
	varTaskID = "a2a.task_id"
	// varContextID carries the remote A2A context id of the current run.
	varContextID = "a2a.context_id"
	// varCard carries the remote AgentCard, when one was available.
	varCard = "a2a.card"
)

// Engine is an A2A remote-proxy [agent.Engine]: one Execute proxies a
// FlowCraft turn to a remote A2A agent and maps the task lifecycle back
// onto the board. It is safe for concurrent use — all per-run state lives
// on the executor, never on the Engine.
type Engine struct {
	client *a2aclient.Client
	card   *a2aprotocol.AgentCard
	opts   Options
}

// New builds an Engine for card. The card must declare at least one
// supported interface; use the config subpackage (or agentcard.Resolver)
// to discover a card from a URL.
func New(ctx context.Context, card *a2aprotocol.AgentCard, opts ...Option) (*Engine, error) {
	if card == nil {
		return nil, errdefs.Validationf("a2a: nil agent card")
	}
	options := applyOptions(opts)
	client, err := newClient(ctx, card, options.ClientOptions)
	if err != nil {
		return nil, classify(err)
	}
	return &Engine{client: client, card: card, opts: options}, nil
}

// Card returns the remote AgentCard the engine was built for, or nil.
func (e *Engine) Card() *a2aprotocol.AgentCard {
	return e.card
}

// Capabilities declares the optional engine behaviours this kind claims.
// The engine supports checkpoint-driven resume, writes checkpoints at task
// boundaries, and may prompt the end user when the remote task reaches
// "input-required".
func (e *Engine) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsResume:  true,
		EmitsCheckpoint: true,
		EmitsUserPrompt: true,
	}
}

// streamEnabled reports whether the current configuration prefers the
// streaming execution path.
func (e *Engine) streamEnabled() bool {
	switch e.opts.StreamMode {
	case StreamModeOn:
		return true
	case StreamModeOff:
		return false
	default:
		return e.card != nil && e.card.Capabilities.Streaming
	}
}

// Execute implements [agent.Engine]. See the package doc for the mapping
// between the A2A task lifecycle and the board / host contract.
func (e *Engine) Execute(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board) (retBoard *agent.Board, runErr error) {
	if board == nil {
		board = agent.NewBoard()
	}
	retBoard = board

	spanAttrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrEngineKind, engineKind),
		attribute.String(telemetry.AttrRunID, run.RunID),
		attribute.String(telemetry.AttrAgentID, run.AgentID),
	}
	if run.TaskID != "" {
		spanAttrs = append(spanAttrs, attribute.String(telemetry.AttrTaskID, run.TaskID))
	}
	ctx, span := telemetry.Tracer().Start(ctx, "a2a.execute", trace.WithAttributes(spanAttrs...))
	started := time.Now()
	defer func() {
		status := execStatus(runErr)
		span.SetAttributes(attribute.String(telemetry.AttrRunStatus, status))
		if runErr != nil && !errdefs.IsInterrupted(runErr) {
			span.RecordError(runErr)
			span.SetStatus(codes.Error, runErr.Error())
		} else {
			span.SetStatus(codes.Ok, status)
		}
		span.End()
		recordExec(ctx, run, status, time.Since(started))
	}()

	_ = publishRunEvent(ctx, host, run, agent.SubjectRunStart(run.RunID), nil)
	defer func() {
		publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runEndPublishTimeout)
		defer cancel()
		if err := publishRunEvent(publishCtx, host, run, agent.SubjectRunEnd(run.RunID), runErr); err != nil {
			a2aPublishErrors.Add(ctx, 1, metric.WithAttributes(
				attribute.String("event.kind", "run_end"),
				attribute.String(telemetry.AttrRunID, run.RunID),
			))
		}
	}()

	if e.card != nil {
		board.SetVar(varCard, e.card)
	}

	x := &executor{
		eng:       e,
		client:    e.client,
		run:       run,
		host:      host,
		board:     board,
		seen:      make(map[string]bool),
		artifacts: make(map[a2aprotocol.ArtifactID]*artifactBuffer),
		streaming: e.streamEnabled(),
	}

	stepActor := stepActorFor(run.AgentID)
	publishStepEvent(ctx, host, run, agent.SubjectStepStart(run.RunID, stepActor), nil)
	if cp := run.ResumeFrom; cp != nil {
		runErr = x.resume(ctx, cp)
	} else {
		runErr = x.runTurn(ctx)
	}
	if runErr != nil {
		publishStepEvent(ctx, host, run, agent.SubjectStepError(run.RunID, stepActor), runErr)
	} else {
		publishStepEvent(ctx, host, run, agent.SubjectStepComplete(run.RunID, stepActor), nil)
	}
	return retBoard, runErr
}

// ---------- executor ----------

// executor carries all per-run state for one Execute. It is never shared
// across runs.
type executor struct {
	eng       *Engine
	client    *a2aclient.Client
	run       agent.Run
	host      agent.Host
	board     *agent.Board
	taskID    a2aprotocol.TaskID
	context   string
	seen      map[string]bool
	artifacts map[a2aprotocol.ArtifactID]*artifactBuffer
	streaming bool
	// promptedInput guards against re-prompting the user while a reply to
	// an input-required state is still in flight.
	promptedInput bool
	// lastState is the most recently observed task state; checkpoints are
	// stamped on state transitions.
	lastState a2aprotocol.TaskState
}

// artifactBuffer accumulates artifact chunks keyed by artifact id until
// the final chunk (or task completion) arrives.
type artifactBuffer struct {
	id    a2aprotocol.ArtifactID
	name  string
	parts []*a2aprotocol.Part
}

// run executes a fresh turn (no resume): it sends the board's user message
// to the remote agent and drives the task to a terminal state.
func (x *executor) runTurn(ctx context.Context) error {
	userMsg, ok := lastUserMessage(x.board)
	if !ok {
		// Nothing to proxy: an engine invoked with an empty board completes
		// as a clean no-op rather than failing.
		return nil
	}
	parts, err := a2aPartsFromMessage(userMsg.Content.Parts)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errdefs.Validationf("a2a: user message has no A2A-representable parts")
	}

	msg := &a2aprotocol.Message{
		ID:        a2aprotocol.NewMessageID(),
		Role:      a2aprotocol.MessageRoleUser,
		Parts:     parts,
		TaskID:    a2aprotocol.TaskID(x.run.TaskID),
		ContextID: x.run.ConversationID,
	}
	req := &a2aprotocol.SendMessageRequest{
		Message: msg,
		Config: &a2aprotocol.SendMessageConfig{
			AcceptedOutputModes: x.eng.opts.AcceptedOutputModes,
			HistoryLength:       x.eng.opts.HistoryLength,
		},
	}

	if x.streaming {
		return x.runStream(ctx, req)
	}
	return x.runPoll(ctx, req)
}

// streamResult is one item read from the remote stream by the reader
// goroutine.
type streamResult struct {
	ev  a2aprotocol.Event
	err error
}

// runStream drives a message/stream exchange. The SDK client falls back to
// message/send when the card does not advertise streaming, so the event
// loop handles both shapes.
//
// Events are read by a dedicated goroutine into a channel so the main loop
// can observe context cancellation and cooperative interrupts even while
// the SSE connection is idle; the executor itself stays single-threaded.
func (x *executor) runStream(ctx context.Context, req *a2aprotocol.SendMessageRequest) error {
	ctx = x.eng.opts.rpcCtx(ctx)
	events := make(chan streamResult, 1)
	go func() {
		defer close(events)
		for ev, err := range x.client.SendStreamingMessage(ctx, req) {
			events <- streamResult{ev: ev, err: err}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case intr := <-x.host.Interrupts():
			x.cancelRemote()
			return agent.Interrupted(intr)
		case sr, ok := <-events:
			if !ok {
				// The stream closed without a terminal state. If no task
				// was attached the remote answered with a direct message
				// and we are done; otherwise the task continues server-side
				// and we re-attach by polling.
				if x.taskID == "" {
					return nil
				}
				return x.runPollAttach(ctx)
			}
			if sr.err != nil {
				return classify(sr.err)
			}
			done, err := x.handleEvent(ctx, sr.ev)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// runPoll drives the non-streaming path: message/send followed by tasks/get
// polling until the task reaches a terminal or interrupted state.
func (x *executor) runPoll(ctx context.Context, req *a2aprotocol.SendMessageRequest) error {
	res, err := x.client.SendMessage(x.eng.opts.rpcCtx(ctx), req)
	if err != nil {
		return classify(err)
	}
	switch v := res.(type) {
	case *a2aprotocol.Message:
		// Direct reply: the remote answered without task tracking.
		if _, err := x.handleMessage(ctx, v); err != nil {
			return err
		}
		return nil
	case *a2aprotocol.Task:
		done, err := x.handleTask(ctx, v)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		return x.runPollAttach(ctx)
	default:
		return errdefs.Internalf("a2a: unexpected message/send result %T", res)
	}
}

// runPollAttach polls the attached task until completion, propagating
// context cancellation and cooperative interrupts.
func (x *executor) runPollAttach(ctx context.Context) error {
	ticker := time.NewTicker(x.eng.opts.PollInterval)
	defer ticker.Stop()
	for {
		task, err := x.client.GetTask(x.eng.opts.rpcCtx(ctx), &a2aprotocol.GetTaskRequest{
			ID:            x.taskID,
			HistoryLength: x.eng.opts.HistoryLength,
		})
		if err != nil {
			return classify(err)
		}
		done, err := x.handleTask(ctx, task)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if err := x.wait(ctx, ticker); err != nil {
			return err
		}
	}
}

// wait blocks until the next poll tick, context cancellation, or a
// cooperative interrupt. A nil ticker only watches ctx / interrupts (the
// streaming path). On interrupt the remote task is cancelled best-effort
// and an [agent.Interrupted] error is returned.
func (x *executor) wait(ctx context.Context, ticker *time.Ticker) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case intr := <-x.host.Interrupts():
		x.cancelRemote()
		return agent.Interrupted(intr)
	case <-tickerC(ticker):
		return nil
	}
}

func tickerC(ticker *time.Ticker) <-chan time.Time {
	if ticker == nil {
		return nil
	}
	return ticker.C
}

// cancelRemote best-effort cancels the attached remote task.
func (x *executor) cancelRemote() {
	if x.taskID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	_, _ = x.client.CancelTask(x.eng.opts.rpcCtx(ctx), &a2aprotocol.CancelTaskRequest{ID: x.taskID})
}

// handleEvent dispatches one streaming event.
func (x *executor) handleEvent(ctx context.Context, ev a2aprotocol.Event) (bool, error) {
	switch v := ev.(type) {
	case *a2aprotocol.Message:
		return x.handleMessage(ctx, v)
	case *a2aprotocol.Task:
		return x.handleTask(ctx, v)
	case *a2aprotocol.TaskStatusUpdateEvent:
		return x.handleStatusUpdate(ctx, v)
	case *a2aprotocol.TaskArtifactUpdateEvent:
		x.handleArtifactUpdate(v)
		return false, nil
	default:
		return false, nil
	}
}

// handleMessage appends one remote agent message to the board. It never
// implies task completion on its own; the streaming loop ends when the
// stream closes or a terminal status arrives.
func (x *executor) handleMessage(ctx context.Context, m *a2aprotocol.Message) (bool, error) {
	if m == nil || m.Role != a2aprotocol.MessageRoleAgent {
		return false, nil
	}
	if m.ID != "" {
		if x.seen[m.ID] {
			return false, nil
		}
		x.seen[m.ID] = true
	}
	x.appendAgentMessage(ctx, m)
	return false, nil
}

// handleTask records task identity, appends unseen history messages and
// buffers artifacts, then drives the state machine.
func (x *executor) handleTask(ctx context.Context, t *a2aprotocol.Task) (bool, error) {
	if t == nil {
		return false, nil
	}
	x.attach(t)
	for _, m := range t.History {
		if m == nil || m.Role != a2aprotocol.MessageRoleAgent {
			continue
		}
		if m.ID != "" && x.seen[m.ID] {
			continue
		}
		if m.ID != "" {
			x.seen[m.ID] = true
		}
		x.appendAgentMessage(ctx, m)
	}
	for _, a := range t.Artifacts {
		x.bufferArtifact(a, true)
	}
	return x.handleState(ctx, t.Status.State, &t.Status)
}

// handleStatusUpdate records task identity and drives the state machine.
func (x *executor) handleStatusUpdate(ctx context.Context, u *a2aprotocol.TaskStatusUpdateEvent) (bool, error) {
	if u.TaskID != "" {
		x.taskID = u.TaskID
	}
	if u.ContextID != "" {
		x.context = u.ContextID
	}
	return x.handleState(ctx, u.Status.State, &u.Status)
}

// handleState maps a remote task state onto the FlowCraft outcome model.
// It returns done=true when the run can finish (terminal states) or an
// error when the state is a failure the host must see.
func (x *executor) handleState(ctx context.Context, state a2aprotocol.TaskState, status *a2aprotocol.TaskStatus) (bool, error) {
	if state != x.lastState {
		x.lastState = state
		x.checkpoint(ctx)
	}
	switch state {
	case a2aprotocol.TaskStateCompleted:
		x.flushArtifacts()
		return true, nil
	case a2aprotocol.TaskStateFailed:
		x.flushArtifacts()
		return true, x.failedError(status)
	case a2aprotocol.TaskStateCanceled:
		x.flushArtifacts()
		return true, agent.Interrupted(agent.Interrupt{
			Cause:  agent.CauseCustom,
			Detail: "a2a: remote task canceled",
		})
	case a2aprotocol.TaskStateRejected:
		x.flushArtifacts()
		return true, errdefs.Validationf("a2a: remote agent rejected the task")
	case a2aprotocol.TaskStateAuthRequired:
		return true, errdefs.Unauthorizedf("a2a: remote agent requires authentication")
	case a2aprotocol.TaskStateInputRequired:
		if x.promptedInput {
			// A reply is already in flight; wait for the server to move on.
			return false, nil
		}
		x.promptedInput = true
		return x.handleInputRequired(ctx, status)
	default:
		x.promptedInput = false
		return false, nil
	}
}

// handleInputRequired bridges an A2A "input-required" state to the host's
// user prompt: the remote question is appended to the transcript, the host
// is asked for a reply, and the reply is sent back to the same task.
func (x *executor) handleInputRequired(ctx context.Context, status *a2aprotocol.TaskStatus) (bool, error) {
	var promptParts []message.Part
	if status != nil && status.Message != nil {
		x.appendAgentMessage(ctx, status.Message)
		promptParts = messagePartsFromA2A(status.Message.Parts)
	}
	reply, err := x.host.AskUser(ctx, agent.UserPrompt{
		Parts:  promptParts,
		Source: "a2a",
	})
	if err != nil {
		return false, err
	}
	parts, err := a2aPartsFromMessage(reply.Parts)
	if err != nil {
		return false, err
	}
	if len(parts) == 0 {
		return false, errdefs.Validationf("a2a: user reply has no A2A-representable parts")
	}
	msg := &a2aprotocol.Message{
		ID:        a2aprotocol.NewMessageID(),
		Role:      a2aprotocol.MessageRoleUser,
		TaskID:    x.taskID,
		ContextID: x.context,
		Parts:     parts,
	}
	// Keep the local transcript complete: the reply becomes a user turn.
	x.board.AppendChannelMessage(agent.MainChannel, message.Message{
		Role:    message.RoleUser,
		Content: message.Content{Parts: reply.Parts},
	})
	res, err := x.client.SendMessage(x.eng.opts.rpcCtx(ctx), &a2aprotocol.SendMessageRequest{Message: msg})
	if err != nil {
		return false, classify(err)
	}
	if task, ok := res.(*a2aprotocol.Task); ok {
		x.attach(task)
	}
	return false, nil
}

// handleArtifactUpdate accumulates artifact chunks by id, honouring the
// append / lastChunk flags.
func (x *executor) handleArtifactUpdate(u *a2aprotocol.TaskArtifactUpdateEvent) {
	if u == nil || u.Artifact == nil {
		return
	}
	buf, ok := x.artifacts[u.Artifact.ID]
	if !ok || !u.Append {
		buf = &artifactBuffer{id: u.Artifact.ID, name: u.Artifact.Name}
		x.artifacts[u.Artifact.ID] = buf
	}
	buf.parts = append(buf.parts, u.Artifact.Parts...)
	if u.LastChunk {
		x.flushArtifact(buf)
	}
}

// bufferArtifact records a complete artifact from a task snapshot. Already
// buffered (or flushed) artifacts are skipped so polling does not duplicate
// them.
func (x *executor) bufferArtifact(a *a2aprotocol.Artifact, final bool) {
	if a == nil {
		return
	}
	if _, exists := x.artifacts[a.ID]; exists {
		return
	}
	buf := &artifactBuffer{id: a.ID, name: a.Name, parts: append([]*a2aprotocol.Part(nil), a.Parts...)}
	x.artifacts[a.ID] = buf
	if final {
		x.flushArtifact(buf)
	}
}

// flushArtifact emits one buffered artifact as an assistant message on the
// main channel.
func (x *executor) flushArtifact(buf *artifactBuffer) {
	if buf == nil {
		return
	}
	parts := messagePartsFromA2A(buf.parts)
	if len(parts) > 0 {
		x.board.AppendChannelMessage(agent.MainChannel, message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		})
	}
	delete(x.artifacts, buf.id)
}

// flushArtifacts emits whatever artifacts remain buffered at task
// completion (streams can end before an explicit lastChunk).
func (x *executor) flushArtifacts() {
	for id, buf := range x.artifacts {
		parts := messagePartsFromA2A(buf.parts)
		if len(parts) > 0 {
			x.board.AppendChannelMessage(agent.MainChannel, message.Message{
				Role:    message.RoleAssistant,
				Content: message.Content{Parts: parts},
			})
		}
		delete(x.artifacts, id)
	}
}

// attach records task identity on the executor and the board.
func (x *executor) attach(t *a2aprotocol.Task) {
	if t.ID != "" {
		x.taskID = t.ID
	}
	if t.ContextID != "" {
		x.context = t.ContextID
	}
	x.board.SetVar(varTaskID, string(x.taskID))
	if x.context != "" {
		x.board.SetVar(varContextID, x.context)
	}
}

// appendAgentMessage converts and appends one remote agent message to the
// main channel, publishing token-level stream deltas on the streaming path.
func (x *executor) appendAgentMessage(ctx context.Context, m *a2aprotocol.Message) {
	parts := messagePartsFromA2A(m.Parts)
	if len(parts) == 0 {
		return
	}
	x.board.AppendChannelMessage(agent.MainChannel, message.Message{
		Role:    message.RoleAssistant,
		Content: message.Content{Parts: parts},
	})
	if x.streaming {
		for _, p := range parts {
			if tp, ok := p.(message.TextPart); ok {
				_ = agent.EmitStreamDelta(ctx, x.host, x.run.RunID,
					stepActorFor(x.run.AgentID),
					agent.StreamDeltaPayload{Type: agent.StreamDeltaPart, Part: tp})
			}
		}
	}
}

// failedError builds a classified error for a failed remote task, carrying
// the remote status message when present.
func (x *executor) failedError(status *a2aprotocol.TaskStatus) error {
	detail := ""
	if status != nil {
		detail = messageText(status.Message)
	}
	if detail == "" {
		return errdefs.Internalf("a2a: remote task %q failed", x.taskID)
	}
	return errdefs.Internalf("a2a: remote task %q failed: %s", x.taskID, detail)
}

// checkpoint stamps a resumable checkpoint at a task boundary. Failures are
// logged and swallowed: checkpoints are advisory, never control flow.
func (x *executor) checkpoint(ctx context.Context) {
	if x.taskID == "" {
		return
	}
	payload, err := json.Marshal(resumePayload{
		TaskID:    string(x.taskID),
		ContextID: x.context,
		Seen:      seenIDs(x.seen),
	})
	if err != nil {
		telemetry.Warn(ctx, "a2a: failed to encode checkpoint payload", otellog.String("error", err.Error()))
		return
	}
	cp := agent.Checkpoint{
		ExecID:     x.run.RunID,
		Steps:      []string{string(x.taskID)},
		Board:      x.board.Snapshot(),
		Payload:    payload,
		Attributes: x.run.Attributes,
		Timestamp:  time.Now(),
	}
	if rc, ok := agent.ResumeContextFromContext(ctx); ok && !rc.StartedAt.IsZero() {
		cp.OriginalStartedAt = rc.StartedAt
	} else {
		cp.OriginalStartedAt = time.Now()
	}
	if err := x.host.Checkpoint(ctx, cp); err != nil {
		telemetry.Warn(ctx, "a2a: checkpoint rejected by host", otellog.String("error", err.Error()))
	}
}

// ---------- resume ----------

// resumePayload is the engine-private checkpoint payload: the remote task
// identity plus the message ids already transcribed, so a resumed run can
// re-attach without duplicating transcript messages.
type resumePayload struct {
	TaskID    string   `json:"task_id"`
	ContextID string   `json:"context_id,omitempty"`
	Seen      []string `json:"seen,omitempty"`
}

// CanResume implements [agent.Resumer]. It is a cheap, local-only probe:
// the checkpoint envelope must validate and the payload must decode and
// name a task. Whether the remote task is still live is a runtime fact
// checked during Execute.
func (e *Engine) CanResume(cp agent.Checkpoint) error {
	if err := cp.Validate(); err != nil {
		return err
	}
	if len(cp.Payload) == 0 {
		return errdefs.Validationf("a2a: checkpoint carries no engine payload")
	}
	var p resumePayload
	if err := json.Unmarshal(cp.Payload, &p); err != nil {
		return errdefs.Validationf("a2a: checkpoint payload: %v", err)
	}
	if p.TaskID == "" {
		return errdefs.Validationf("a2a: checkpoint payload missing task id")
	}
	return nil
}

// resume re-attaches to a previously started remote task: the board is
// restored from the checkpoint and the engine subscribes (streaming) or
// polls (non-streaming) until the task reaches a terminal state.
func (x *executor) resume(ctx context.Context, cp *agent.Checkpoint) error {
	if err := cp.Validate(); err != nil {
		return err
	}
	if cp.ExecID != x.run.RunID {
		return errdefs.Validationf("a2a: checkpoint exec id %q != run id %q (forking requires a fresh run)",
			cp.ExecID, x.run.RunID)
	}
	x.board.RestoreFrom(cp.Board)
	var p resumePayload
	if err := json.Unmarshal(cp.Payload, &p); err != nil {
		return errdefs.Validationf("a2a: checkpoint payload: %v", err)
	}
	if p.TaskID == "" {
		return errdefs.Validationf("a2a: checkpoint payload missing task id")
	}
	x.taskID = a2aprotocol.TaskID(p.TaskID)
	x.context = p.ContextID
	for _, id := range p.Seen {
		x.seen[id] = true
	}
	x.board.SetVar(varTaskID, string(x.taskID))
	if x.context != "" {
		x.board.SetVar(varContextID, x.context)
	}

	if x.streaming {
		ctx = x.eng.opts.rpcCtx(ctx)
		for ev, err := range x.client.SubscribeToTask(ctx, &a2aprotocol.SubscribeToTaskRequest{ID: x.taskID}) {
			if err != nil {
				return classify(err)
			}
			done, err := x.handleEvent(ctx, ev)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			if err := x.wait(ctx, nil); err != nil {
				return err
			}
		}
	}
	return x.runPollAttach(ctx)
}

// ---------- helpers ----------

// lastUserMessage returns the last user turn on the main channel.
func lastUserMessage(board *agent.Board) (message.Message, bool) {
	msgs := board.Channel(agent.MainChannel)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleUser {
			return msgs[i], true
		}
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1], true
	}
	return message.Message{}, false
}

// a2aPartsFromMessage converts FlowCraft parts into A2A parts, skipping
// kinds the A2A part model cannot represent.
func a2aPartsFromMessage(parts []message.Part) ([]*a2aprotocol.Part, error) {
	out := make([]*a2aprotocol.Part, 0, len(parts))
	for _, p := range parts {
		ap, handled, err := a2aPartFromMessagePart(p)
		if err != nil {
			return nil, err
		}
		if handled {
			out = append(out, ap)
		}
	}
	return out, nil
}

func seenIDs(seen map[string]bool) []string {
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func stepActorFor(agentID string) string {
	return agentID + ".remote"
}

// ---------- events ----------

// publishRunEvent mints a run-start / run-end envelope and forwards it to
// the host. Failures are returned so the caller can decide (run-end failures
// are joined into the run error per the engine contract).
func publishRunEvent(ctx context.Context, host agent.Host, run agent.Run, subject event.Subject, runErr error) error {
	if host == nil {
		return nil
	}
	payload := struct {
		Error string `json:"error,omitempty"`
	}{}
	if runErr != nil {
		payload.Error = runErr.Error()
	}
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return err
	}
	env.SetAgentID(run.AgentID)
	env.SetRunID(run.RunID)
	return host.Publish(ctx, env)
}

// publishStepEvent publishes a best-effort step lifecycle envelope for the
// single "remote" step of an A2A run. Publish failures are swallowed (they
// are observability, not control flow).
func publishStepEvent(ctx context.Context, host agent.Host, run agent.Run, subject event.Subject, stepErr error) {
	if host == nil {
		return
	}
	payload := struct {
		Step  string `json:"step"`
		Error string `json:"error,omitempty"`
	}{Step: "remote"}
	if stepErr != nil {
		payload.Error = stepErr.Error()
	}
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return
	}
	env.SetAgentID(run.AgentID)
	env.SetRunID(run.RunID)
	_ = host.Publish(ctx, env)
}

// ---------- telemetry ----------

var (
	a2aMeter = telemetry.MeterWithSuffix(engineKind)

	a2aExecCount, _ = a2aMeter.Int64Counter(
		"executions.total",
		metric.WithDescription("Total a2a proxy executions by status"))
	a2aExecDuration, _ = a2aMeter.Float64Histogram(
		"duration.seconds",
		metric.WithDescription("A2A proxy execution duration"))
	a2aPublishErrors, _ = a2aMeter.Int64Counter(
		"publish_errors.total",
		metric.WithDescription("Event publish failures swallowed by the a2a engine"))
)

func recordExec(ctx context.Context, run agent.Run, status string, dur time.Duration) {
	attrs := metric.WithAttributes(
		attribute.String(telemetry.AttrRunID, run.RunID),
		attribute.String(telemetry.AttrAgentID, run.AgentID),
		attribute.String(telemetry.AttrEngineKind, engineKind),
		attribute.String("status", status),
	)
	a2aExecCount.Add(ctx, 1, attrs)
	a2aExecDuration.Record(ctx, dur.Seconds(), attrs)
}

// execStatus classifies an Execute outcome for spans and metrics.
func execStatus(err error) string {
	switch {
	case err == nil:
		return "success"
	case errdefs.IsInterrupted(err):
		return "interrupted"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	default:
		return "failed"
	}
}

var _ agent.Engine = (*Engine)(nil)
var _ agent.Resumer = (*Engine)(nil)
