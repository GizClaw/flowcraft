package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/telemetry"
	toolengine "github.com/GizClaw/flowcraft/core/tool/engine"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ErrPaused tells the loop that the answer is not available yet. Ask returns it
// when the host cannot answer synchronously; the Task then stops in
// input-required, which is the one state a checkpoint exists for.
var ErrPaused = errors.New("agent: waiting for the user")

// Bounds caps one Task. Zero fields mean "no bound", except MaxRounds where the
// zero value means "until the model stops asking for tools".
type Bounds struct {
	// MaxRounds caps model calls within one Task.
	MaxRounds int
	// MaxToolCalls caps tool executions within one Task.
	MaxToolCalls int
	// Timeout bounds the whole Task.
	Timeout time.Duration
}

// Prompt is a question the Task needs answered before it can continue.
type Prompt struct{ Question string }

// Reply is the answer to a Prompt.
type Reply struct{ Text string }

// Options configures a Session. Nil fields mean "not supported": a Task that
// needs one reports input-required rather than pretending to have it.
type Options struct {
	// Ask lets the Task pause and ask the user. Nil means unsupported.
	Ask func(ctx context.Context, prompt Prompt) (Reply, error)
	// Interrupts is closed or signalled to cancel the running Task.
	Interrupts <-chan struct{}
	// Bounds caps the Task.
	Bounds Bounds
	// Store persists a checkpoint when the Task pauses. Nil means the caller
	// keeps the Session in memory instead.
	Store Store
}

// Option mutates Options at construction.
type Option func(*Options)

// WithAsk installs the user-prompt seam.
func WithAsk(ask func(ctx context.Context, prompt Prompt) (Reply, error)) Option {
	return func(o *Options) { o.Ask = ask }
}

// WithInterrupts installs the cooperative-interrupt channel.
func WithInterrupts(ch <-chan struct{}) Option {
	return func(o *Options) { o.Interrupts = ch }
}

// WithStore installs the checkpoint store used when a Task pauses.
func WithStore(store Store) Option {
	return func(o *Options) { o.Store = store }
}

// WithBounds caps one Task.
func WithBounds(bounds Bounds) Option {
	return func(o *Options) { o.Bounds = bounds }
}

// Mode says what a submitted message wants to do with the running Task.
type Mode int

const (
	// ModeQueue delivers the message as the next Task's input.
	ModeQueue Mode = iota
	// ModeSteer injects the message into the running Task: the model sees it no
	// later than the next round's request.
	ModeSteer
	// ModeInterrupt cancels the running round and leaves the message queued as
	// the next Task's input.
	ModeInterrupt
)

func (m Mode) String() string {
	switch m {
	case ModeSteer:
		return "steer"
	case ModeInterrupt:
		return "interrupt"
	default:
		return "queue"
	}
}

// submission is one message waiting in the mailbox.
type submission struct {
	message message.Message
	mode    Mode
}

// Input is one user request.
type Input struct {
	// System frames the request. It is Content, not a string, because a system
	// prompt is a list of parts like any other message.
	System message.Content
	// Messages are the new turns to add before answering.
	Messages []message.Message
}

// Session runs Tasks against one runtime. It holds the conversation transcript
// and nothing else: no catalog, no view, no cached discovery.
type Session struct {
	runtime *runtime.Runtime
	task    *Task
	opts    Options

	mu sync.Mutex
	// mailbox holds submissions not yet consumed by a Task.
	mailbox []submission
	// running is true between the start and the end of Run.
	running bool
	// interruptRequested is set by an interrupt submission, so the loop can tell
	// its own cancellation from the caller's.
	interruptRequested bool
	// cancelRound cancels the round in flight, if any.
	cancelRound context.CancelFunc
}

// NewSession binds a Session to a runtime. The binding is for life: a Session
// belongs to the runtime it was created with, because everything it tells the
// model about available tools comes from that runtime's catalog.
func NewSession(rt *runtime.Runtime, opts ...Option) (*Session, error) {
	if rt == nil {
		return nil, errdefs.Validationf("agent: a runtime is required")
	}
	session := &Session{runtime: rt, task: &Task{ID: newTaskID(), State: StateSubmitted}}
	for _, opt := range opts {
		if opt != nil {
			opt(&session.opts)
		}
	}
	return session, nil
}

// Send submits one message to the mailbox and reports the mode that will take
// effect.
//
// A message can only steer or interrupt a Task that is running; while the
// Session is idle both degrade to queue, and the returned mode says so, because
// a caller told "steered" when nothing was injected would wait for an effect
// that never comes.
func (s *Session) Send(ctx context.Context, msg message.Message, mode Mode) (Mode, error) {
	if err := ctx.Err(); err != nil {
		return mode, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	effective := mode
	if !s.running {
		effective = ModeQueue
	}
	s.mailbox = append(s.mailbox, submission{message: msg, mode: mode})
	if effective == ModeInterrupt {
		s.interruptRequested = true
		if s.cancelRound != nil {
			s.cancelRound()
		}
	}
	return effective, nil
}

// Pending reports how many submissions are waiting.
func (s *Session) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.mailbox)
}

// Abandon cancels a Task paused on a question. The caller decided not to answer
// it; the Task ends canceled instead of waiting forever.
func (s *Session) Abandon() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.task.State != StateInputRequired {
		return errdefs.Validationf(
			"agent: task %s is %s, not waiting for input", s.task.ID, s.task.State)
	}
	s.task.State = StateCanceled
	s.task.Pending = nil
	return nil
}

// takeQueued removes the submissions the next Task consumes.
func (s *Session) takeQueued() []message.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.mailbox[:0]
	out := make([]message.Message, 0, len(s.mailbox))
	for _, item := range s.mailbox {
		if item.mode == ModeQueue || item.mode == ModeInterrupt {
			out = append(out, item.message)
			continue
		}
		kept = append(kept, item)
	}
	s.mailbox = kept
	return out
}

// drainSteers removes the steering submissions for one round boundary.
func (s *Session) drainSteers() []message.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.mailbox[:0]
	out := make([]message.Message, 0, len(s.mailbox))
	for _, item := range s.mailbox {
		if item.mode == ModeSteer {
			out = append(out, item.message)
			continue
		}
		kept = append(kept, item)
	}
	s.mailbox = kept
	return out
}

// takeAll removes everything left, whatever its mode.
func (s *Session) takeAll() []message.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]message.Message, 0, len(s.mailbox))
	for _, item := range s.mailbox {
		out = append(out, item.message)
	}
	s.mailbox = nil
	return out
}

// consumeInterrupt reports and clears a pending interrupt request.
func (s *Session) consumeInterrupt() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	requested := s.interruptRequested
	s.interruptRequested = false
	return requested
}

func (s *Session) setRoundCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelRound = cancel
}

func (s *Session) markRunning(running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = running
	if !running {
		s.cancelRound = nil
	}
}

// Task returns the Task this Session is running or last ran.
func (s *Session) Task() *Task { return s.task }

// Run executes one Task: one user request, answered after as many model rounds
// as it takes. The returned sequence is the Task's process; a caller that wants
// only the final answer folds it.
func (s *Session) Run(ctx context.Context, in Input) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if err := s.run(ctx, in, yield); err != nil {
			yield(nil, err)
		}
	}
}

func (s *Session) run(ctx context.Context, in Input, yield func(Event, error) bool) error {
	// Queued submissions and the caller's messages are the same thing: user
	// input. A queue entry that lost to an explicit input would be input the
	// user sent and the model never saw.
	messages := append(s.takeQueued(), in.Messages...)
	if len(messages) == 0 {
		return errdefs.Validationf("agent: input has no messages")
	}
	s.markRunning(true)
	defer s.markRunning(false)
	task := s.task
	if task.State.Terminal() {
		// A Session may run several Tasks; a finished one is replaced.
		task = &Task{ID: newTaskID(), State: StateSubmitted}
		s.task = task
	}
	task.System = in.System
	if err := task.advance(StateWorking); err != nil {
		return err
	}
	if !yield(Status{State: StateWorking}, nil) {
		return nil
	}

	ctx, span, cancel := s.begin(ctx, task, "agent.run")
	defer span.End()
	defer cancel()

	view := s.view(ctx, task)
	history := append(task.History, messages...)
	contextMessages := history[:len(history)-len(messages)]
	input := inference.GenerateInput{
		Role:    inference.InputRoleUser,
		Content: messages[len(messages)-1].Content,
	}
	task.History = history
	return s.loop(ctx, task, view, contextMessages, input, 0, yield)
}

// Resume continues a Task that stopped waiting for the user.
//
// The reply becomes the result of the call the Task paused on, which is why the
// pending call is part of the checkpoint: without it the answer would arrive
// with no question to attach it to.
func (s *Session) Resume(ctx context.Context, reply Reply) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if err := s.resume(ctx, reply, yield); err != nil {
			yield(nil, err)
		}
	}
}

func (s *Session) resume(ctx context.Context, reply Reply, yield func(Event, error) bool) error {
	task := s.task
	if task.State != StateInputRequired || task.Pending == nil {
		return errdefs.Validationf("agent: task %s is not waiting for input", task.ID)
	}
	pending := *task.Pending
	result := message.NewTextToolResult(pending.ID, reply.Text)
	observation := toolMessage([]message.ToolResult{result})
	task.History = append(task.History, observation)
	task.Pending = nil
	task.State = StateWorking
	if !yield(ToolResult{Result: result}, nil) {
		return nil
	}

	ctx, span, cancel := s.begin(ctx, task, "agent.resume")
	defer span.End()
	defer cancel()

	view := s.view(ctx, task)
	contextMessages := task.History[:len(task.History)-1]
	input := inference.GenerateInput{Role: inference.InputRoleTool, Content: observation.Content}
	return s.loop(ctx, task, view, contextMessages, input, 0, yield)
}

// begin prepares the context: runtime identity, the Task's root span and the
// deadline. Every span below this one — route attempts, tool calls, sandbox
// commands — nests under it.
func (s *Session) begin(ctx context.Context, task *Task, name string) (context.Context, trace.Span, context.CancelFunc) {
	ctx = s.runtime.Context(ctx)
	// The runtime identity rides on every span below this one; the root has to
	// carry it too, or filtering a trace by runtime would miss its own root.
	attrs := append(runctx.Attrs(ctx),
		attribute.String(telemetry.AttrTaskID, task.ID),
		attribute.String(telemetry.AttrConversationID, task.ContextID),
		attribute.String(telemetry.AttrLLMModel, s.runtime.DefaultModel().ID.Name),
	)
	ctx, span := telemetry.TracerWithSuffix("agent").Start(ctx, name,
		trace.WithAttributes(attrs...))
	cancel := func() {}
	if s.opts.Bounds.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, s.opts.Bounds.Timeout)
	}
	return ctx, span, cancel
}

// view creates the Task-scoped catalog view. It is created here and discarded
// when the Task ends: it is neither a Session field (a cross-task cache the
// checkpoint would have to serialise) nor a caller input (a view belongs to one
// assembly, and tool_search searches the view's catalog while execution uses the
// runtime's registry).
func (s *Session) view(ctx context.Context, task *Task) toolengine.CatalogView {
	view := s.runtime.Assembly().NewCatalogView()
	replayDiscoveries(view, task.History)
	return view
}

// loop runs rounds until the Task ends. It is shared by Run and Resume so the
// two cannot drift.
func (s *Session) loop(
	ctx context.Context,
	task *Task,
	view toolengine.CatalogView,
	contextMessages []message.Message,
	input inference.GenerateInput,
	toolCalls int,
	yield func(Event, error) bool,
) error {
	var roundEvents []EventRecord
	for {
		if s.consumeInterrupt() {
			return s.finish(task, StateCanceled, nil, true, yield)
		}
		if err := s.interrupted(ctx); err != nil {
			return s.finish(task, StateCanceled, err, true, yield)
		}
		roundCtx, cancelRound := context.WithCancel(ctx)
		s.setRoundCancel(cancelRound)
		defs := view.Definitions()
		if s.opts.Ask != nil {
			defs = append(defs, askUserDefinition())
		}
		tools, err := inference.NewToolset(defs)
		if err != nil {
			return s.finish(task, StateFailed, err, true, yield)
		}
		request := inference.GenerateRequest{
			Model:   s.runtime.DefaultModel(),
			Context: contextMessages,
			Input:   input,
			Tools:   tools,
		}
		if !yield(StreamStart{Round: task.Round, Model: request.Model.ID.Name}, nil) {
			return nil
		}
		response, err := s.generate(roundCtx, request, yield)
		if err != nil {
			cancelRound()
			if s.consumeInterrupt() {
				return s.finish(task, StateCanceled, nil, true, yield)
			}
			return s.finish(task, StateFailed, err, true, yield)
		}
		task.History = append(task.History, response.Message)
		roundEvents = append(roundEvents, messageEvent(response.Message))
		if !yield(UsageEvent{Usage: response.Usage}, nil) {
			return nil
		}

		calls := toolCallsOf(response.Message)
		if len(calls) == 0 {
			return s.finish(task, StateCompleted, nil, true, yield)
		}
		if limit := s.opts.Bounds.MaxToolCalls; limit > 0 && toolCalls+len(calls) > limit {
			return s.finish(task, StateFailed,
				errdefs.Validationf("agent: task %s exceeded %d tool calls", task.ID, limit),
				true, yield)
		}
		toolCalls += len(calls)

		results := make([]message.ToolResult, 0, len(calls))
		for _, call := range calls {
			if !yield(ToolCallStart{Call: call}, nil) {
				return nil
			}
			if call.Name == askUserTool {
				result, paused := s.ask(ctx, call)
				if paused {
					return s.pause(ctx, task, call, roundEvents, yield)
				}
				results = append(results, result)
				if !yield(ToolResult{Result: result}, nil) {
					return nil
				}
				continue
			}
			result := s.runtime.Assembly().Execute(roundCtx, call)
			view.RecordCall(call)
			results = append(results, result)
			if !yield(ToolResult{Result: result}, nil) {
				return nil
			}
		}
		observation := toolMessage(results)
		task.History = append(task.History, observation)
		roundEvents = append(roundEvents, messageEvent(observation))
		contextMessages = task.History[:len(task.History)-1]
		input = inference.GenerateInput{Role: inference.InputRoleTool, Content: observation.Content}

		cancelRound()
		view.AdvanceRound()
		task.Round++
		// A round boundary is the only place a checkpoint may be written: a
		// half round holds a tool call whose result nobody has yet.
		s.persistQuietly(ctx, roundEvents)
		roundEvents = nil
		// Steering lands at the round boundary: it changes what the next request
		// asks, so it cannot arrive mid-stream.
		if steers := s.drainSteers(); len(steers) > 0 {
			task.History = append(task.History, steers...)
			contextMessages = task.History[:len(task.History)-1]
			input = inference.GenerateInput{
				Role:    inference.InputRoleUser,
				Content: steers[len(steers)-1].Content,
			}
			if !yield(SteerApplied{Count: len(steers), Messages: steers}, nil) {
				return nil
			}
		}
		if max := s.opts.Bounds.MaxRounds; max > 0 && task.Round >= max {
			return s.finish(task, StateCompleted, nil, true, yield)
		}
	}
}

// pause stops the Task in input-required and checkpoints it.
func (s *Session) pause(
	ctx context.Context,
	task *Task,
	call message.ToolCall,
	events []EventRecord,
	yield func(Event, error) bool,
) error {
	pending := call
	task.Pending = &pending
	task.State = StateInputRequired
	// Pausing without a resume point would lose the Task, so here a store
	// failure fails the Task instead of being reported as lag.
	if err := s.persist(ctx, events); err != nil {
		return s.finish(task, StateFailed, err, true, yield)
	}
	if !yield(Status{State: StateInputRequired, Reason: "waiting for the user"}, nil) {
		return nil
	}
	return nil
}

// ask answers one ask_user call. The second return value reports that the host
// could not answer yet, which turns the Task into a paused one.
func (s *Session) ask(ctx context.Context, call message.ToolCall) (message.ToolResult, bool) {
	if s.opts.Ask == nil {
		return message.NewErrorToolResult(call.ID,
			"asking the user is not supported by this host"), false
	}
	var args struct {
		Question string `json:"question"`
	}
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return message.NewErrorToolResult(call.ID, "question is not valid JSON: "+err.Error()), false
		}
	}
	reply, err := s.opts.Ask(ctx, Prompt{Question: args.Question})
	if errors.Is(err, ErrPaused) {
		return message.ToolResult{}, true
	}
	if err != nil {
		return message.NewErrorToolResult(call.ID, err.Error()), false
	}
	return message.NewTextToolResult(call.ID, reply.Text), false
}

// askUserTool is the name the loop intercepts. It is offered to the model only
// when the host can answer it.
const askUserTool = "ask_user"

func askUserDefinition() message.ToolDefinition {
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "The question to put to the user.",
			},
		},
		"required":             []string{"question"},
		"additionalProperties": false,
	})
	return message.ToolDefinition{
		Name:        askUserTool,
		Description: "Ask the user a question and wait for the answer. Use it only when the task cannot proceed without a decision or missing information.",
		InputSchema: schema,
	}
}

// generate drains one model stream, forwarding text, and returns the final
// response, which carries the assembled tool calls.
func (s *Session) generate(
	ctx context.Context,
	request inference.GenerateRequest,
	yield func(Event, error) bool,
) (inference.GenerateResponse, error) {
	stream, err := s.runtime.Provider().GenerateStream(ctx, request)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	defer func() { _ = stream.Close() }()

	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		switch event.Kind {
		case inference.StreamText:
			if !yield(Text{Text: event.Text}, nil) {
				return inference.GenerateResponse{}, nil
			}
		case inference.StreamReasoning:
			if !yield(Reasoning{Text: event.Text}, nil) {
				return inference.GenerateResponse{}, nil
			}
		}
	}
	return stream.Result()
}

func (s *Session) interrupted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.opts.Interrupts == nil {
		return nil
	}
	select {
	case <-s.opts.Interrupts:
		return errdefs.Validationf("agent: task interrupted")
	default:
		return nil
	}
}

// finish moves the Task to a terminal state and reports it.
func (s *Session) finish(
	task *Task,
	state TaskState,
	cause error,
	report bool,
	yield func(Event, error) bool,
) error {
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	task.State = state
	if state.Terminal() {
		// A paused Task keeps its mailbox: those entries are for the next Run.
		// A finished one hands the rest up, because only the caller knows whether
		// the conversation continues or the client is gone.
		if leftovers := s.takeAll(); len(leftovers) > 0 {
			if !yield(Undelivered{Messages: leftovers}, nil) {
				return nil
			}
		}
	}
	if report {
		if !yield(Status{State: state, Reason: reason}, nil) {
			return nil
		}
	}
	return cause
}

// messageEvent records one appended message.
func messageEvent(msg message.Message) EventRecord {
	body, err := json.Marshal(msg)
	if err != nil {
		return EventRecord{Kind: "message", Payload: json.RawMessage(`{}`)}
	}
	return EventRecord{Kind: "message", Payload: body}
}

// persist writes the projection and this round's events.
func (s *Session) persist(ctx context.Context, events []EventRecord) error {
	if s.opts.Store == nil {
		return nil
	}
	return s.opts.Store.Save(ctx, s.Snapshot(), events)
}

// persistQuietly writes a round-boundary checkpoint without failing the Task: a
// checkpoint that lags is a resume problem, not a reason to abort an answer the
// model already produced.
func (s *Session) persistQuietly(ctx context.Context, events []EventRecord) {
	if err := s.persist(ctx, events); err != nil {
		telemetry.WarnErr(ctx, "agent: checkpoint write failed", err)
	}
}

// toolCallsOf extracts the tool calls the model asked for.
func toolCallsOf(msg message.Message) []message.ToolCall {
	calls := make([]message.ToolCall, 0, len(msg.Content.Parts))
	for _, part := range msg.Content.Parts {
		if call, ok := part.(message.ToolCallPart); ok {
			calls = append(calls, call.Call)
		}
	}
	return calls
}

// toolMessage packs every result of one round into a single tool turn, which is
// what the next request carries as its input.
func toolMessage(results []message.ToolResult) message.Message {
	parts := make([]message.Part, 0, len(results))
	for _, result := range results {
		parts = append(parts, message.ToolResultPart{Result: result})
	}
	return message.Message{Role: message.RoleTool, Content: message.Content{Parts: parts}}
}

// replayDiscoveries restores the tool visibility of a Task from its transcript.
//
// Visibility is a function of history, not stored state: a conversation that
// called read_file keeps read_file visible without a checkpoint having to record
// a discovery pool. Names the catalog no longer has are ignored, and the same
// budget that applies during the loop applies here.
func replayDiscoveries(view toolengine.CatalogView, history []message.Message) {
	names := make([]string, 0, 8)
	for _, msg := range history {
		for _, call := range toolCallsOf(msg) {
			if call.Name == "" {
				continue
			}
			names = append(names, call.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	view.Discover(names...)
}
