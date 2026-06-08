package claw

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
)

// EventType is a publish Claw stream event discriminator.
type EventType string

const (
	EventToken      EventType = "token"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventResult     EventType = "result"
	EventError      EventType = "error"
)

// Request is one user turn.
type Request struct {
	Context context.Context
	Text    string
	Inputs  map[string]any
}

// Event is one item emitted by a Response iterator.
type Event struct {
	Type        EventType     `json:"type"`
	Content     string        `json:"content,omitempty"`
	ID          string        `json:"id,omitempty"`
	Name        string        `json:"name,omitempty"`
	Arguments   any           `json:"arguments,omitempty"`
	ToolCallID  string        `json:"tool_call_id,omitempty"`
	IsError     bool          `json:"is_error,omitempty"`
	NodeID      string        `json:"node_id,omitempty"`
	AgentID     string        `json:"agent_id,omitempty"`
	ForkID      string        `json:"fork_id,omitempty"`
	BranchID    string        `json:"branch_id,omitempty"`
	Speculative bool          `json:"speculative,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	Result      *agent.Result `json:"result,omitempty"`
	Err         string        `json:"error,omitempty"`
}

// Response is a blocking iterator over one round trip's events.
type Response struct {
	events <-chan Event
	round  *roundController
}

// Next returns the next event. It returns io.EOF after the round trip ends.
func (r *Response) Next() (Event, error) {
	if r == nil {
		return Event{}, io.EOF
	}
	ev, ok := <-r.events
	if !ok {
		return Event{}, io.EOF
	}
	if r.round != nil {
		r.round.recordRead(ev)
	}
	return ev, nil
}

// Interrupt asks the running round trip to stop. When discard is false, Claw
// commits the assistant text that had already been read from this Response.
func (r *Response) Interrupt(discard bool) error {
	if r == nil || r.round == nil {
		return io.EOF
	}
	r.round.interrupt(discard)
	return nil
}

// RoundTrip starts one user turn. A newer turn interrupts the previous active
// turn for the same Claw instance.
func (c *Claw) RoundTrip(req Request) (*Response, error) {
	if c == nil {
		return nil, errdefs.Validationf("claw: nil Claw")
	}
	id := c.contextID()
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	round := newRoundController(c, id, req.Text, cancel)

	c.mu.Lock()
	if prev := c.active; prev != nil {
		prev.interrupt(true)
	}
	c.active = round
	c.mu.Unlock()

	events := make(chan Event, 32)
	resp := &Response{events: events, round: round}
	go c.runRoundTrip(ctx, id, req, events, round)
	return resp, nil
}

func (c *Claw) runRoundTrip(ctx context.Context, id string, req Request, events chan<- Event, round *roundController) {
	defer func() {
		if recovered := recover(); recovered != nil {
			select {
			case events <- Event{Type: EventError, Err: fmt.Sprintf("panic: %v", recovered), IsError: true}:
			default:
			}
		}
		c.mu.Lock()
		if c.active == round {
			c.active = nil
		}
		c.mu.Unlock()
		round.finish()
		close(events)
	}()

	host := engine.HostFuncs{
		Inner: engine.NoopHost{},
		InterruptsFn: func() <-chan engine.Interrupt {
			return round.interrupts()
		},
	}
	streams := newRoundStreamMux(ctx, events, c.cfg.Agent.Publisher)
	host.PublishFn = streams.Publish
	result, err := agent.Run(ctx, c.agent, c.engine, agent.Request{
		ContextID: id,
		Message:   model.NewTextMessage(model.RoleUser, req.Text),
		Inputs:    req.Inputs,
	},
		agent.WithEngineHost(host),
		agent.WithBoardSeed(c.boardSeeder()),
		agent.WithDependencies(c.dependencies()),
	)
	if err != nil {
		events <- Event{Type: EventError, Err: err.Error(), IsError: true}
		return
	}
	if result != nil && result.Err != nil {
		if errdefs.IsInterrupted(result.Err) && round.shouldCommitPartial() {
			if err := round.commitPartial(ctx); err != nil {
				events <- Event{Type: EventError, Err: err.Error(), IsError: true, Result: result}
				return
			}
		}
		events <- Event{Type: EventError, Err: result.Err.Error(), IsError: true, Result: result}
		return
	}
	if c.memory != nil {
		var boardVars map[string]any
		if result != nil && result.LastBoard != nil {
			boardVars = result.LastBoard.Vars()
		}
		if err := c.memory.saveTurn(ctx, id, req.Text, latestAssistant(result.Messages), boardVars); err != nil {
			events <- Event{Type: EventError, Err: err.Error(), IsError: true, Result: result}
			return
		}
	}
	if c.history != nil {
		var assistantMessages []model.Message
		if result != nil {
			assistantMessages = result.Messages
		}
		if err := c.history.appendTurn(ctx, id, model.NewTextMessage(model.RoleUser, req.Text), assistantMessages); err != nil {
			events <- Event{Type: EventError, Err: err.Error(), IsError: true, Result: result}
			return
		}
	}
	if err := c.saveContextState(ctx, id, result, req.Inputs); err != nil {
		events <- Event{Type: EventError, Err: err.Error(), IsError: true, Result: result}
		return
	}
	events <- Event{Type: EventResult, Result: result}
}

func (c *Claw) boardSeeder() agent.BoardSeeder {
	return agent.BoardSeederFunc(func(ctx context.Context, _ agent.RunInfo, req *agent.Request) (*engine.Board, error) {
		board := engine.NewBoard()
		st, err := c.loadContextState(ctx, req.ContextID)
		if err != nil {
			return nil, fmt.Errorf("claw: load context state: %w", err)
		}
		for k, v := range st.Vars {
			board.SetVar(k, v)
		}
		if len(st.Vars) > 0 {
			board.SetVar(workspaceStateVar, st.Vars)
		}
		if c.history != nil {
			prior, err := c.history.load(ctx, req.ContextID)
			if err != nil {
				return nil, fmt.Errorf("claw: load history: %w", err)
			}
			if len(prior) > 0 {
				board.SetChannel(engine.MainChannel, prior)
			}
		}
		if c.memory != nil {
			memVars, err := c.memory.recallBoardVars(ctx, req.Message.Content())
			if err != nil {
				return nil, fmt.Errorf("claw: recall memory: %w", err)
			}
			for key, value := range memVars {
				board.SetVar(key, value)
			}
		}
		board.AppendChannelMessage(engine.MainChannel, req.Message)
		for k, v := range req.Inputs {
			board.SetVar(k, v)
		}
		return board, nil
	})
}

func (c *Claw) dependencies() *engine.Dependencies {
	deps := engine.NewDependencies()
	deps.Set(depname.LLMResolver, c.resolver)
	if c.tools != nil {
		deps.Set(depname.ToolRegistry, c.tools)
	}
	return deps
}

func latestAssistant(msgs []model.Message) model.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant {
			return msgs[i]
		}
	}
	return model.Message{}
}

func eventFromEnvelope(env event.Envelope) (Event, bool, error) {
	if !engine.IsStreamDelta(env.Subject) {
		return Event{}, false, nil
	}
	delta, err := engine.DecodeStreamDelta(env)
	if err != nil {
		return Event{}, false, fmt.Errorf("claw: decode stream delta: %w", err)
	}
	ev := Event{
		Type:        EventType(delta.Type),
		Content:     delta.Content,
		ID:          delta.ID,
		Name:        delta.Name,
		Arguments:   delta.Arguments,
		ToolCallID:  delta.ToolCallID,
		IsError:     delta.IsError,
		NodeID:      env.NodeID(),
		AgentID:     env.AgentID(),
		ForkID:      delta.ForkID,
		BranchID:    delta.BranchID,
		Speculative: delta.Speculative,
		Reason:      delta.Reason,
	}
	switch delta.Type {
	case engine.StreamDeltaToken:
		ev.Type = EventToken
	case engine.StreamDeltaToolCall:
		ev.Type = EventToolCall
	case engine.StreamDeltaToolResult:
		ev.Type = EventToolResult
	}
	return ev, true, nil
}

type roundStreamMux struct {
	ctx    context.Context
	events chan<- Event
	policy PublisherConfig

	mu       sync.Mutex
	buffers  map[string][]Event
	terminal map[string]bool
}

func newRoundStreamMux(ctx context.Context, events chan<- Event, policy PublisherConfig) *roundStreamMux {
	return &roundStreamMux{
		ctx:      ctx,
		events:   events,
		policy:   policy,
		buffers:  make(map[string][]Event),
		terminal: make(map[string]bool),
	}
}

func (m *roundStreamMux) Publish(_ context.Context, env event.Envelope) error {
	ev, ok, err := eventFromEnvelope(env)
	if err != nil {
		return m.send(Event{Type: EventError, Err: err.Error(), IsError: true})
	}
	if !ok {
		return nil
	}
	if !m.shouldPublish(ev) {
		return nil
	}

	key := streamBranchKey(ev)
	if key == "" {
		return m.send(ev)
	}

	switch ev.Type {
	case EventType(engine.StreamDeltaParallelBranchAccept):
		return m.acceptBranch(key)
	case EventType(engine.StreamDeltaParallelBranchCancel):
		m.cancelBranch(key)
		return nil
	default:
		if ev.Speculative {
			m.bufferBranchEvent(key, ev)
			return nil
		}
		return m.send(ev)
	}
}

func (m *roundStreamMux) shouldPublish(ev Event) bool {
	switch ev.Type {
	case EventToken, EventToolCall, EventToolResult:
	default:
		return true
	}
	if ev.NodeID == "" || m == nil || m.policy.Nodes == nil {
		return false
	}
	policy, ok := m.policy.Nodes[ev.NodeID]
	if !ok || policy.Publish == nil {
		return false
	}
	return *policy.Publish
}

func streamBranchKey(ev Event) string {
	if ev.ForkID == "" || ev.BranchID == "" {
		return ""
	}
	return ev.ForkID + "\x00" + ev.BranchID
}

func (m *roundStreamMux) bufferBranchEvent(key string, ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.terminal[key] {
		return
	}
	m.buffers[key] = append(m.buffers[key], ev)
}

func (m *roundStreamMux) acceptBranch(key string) error {
	m.mu.Lock()
	if m.terminal[key] {
		m.mu.Unlock()
		return nil
	}
	m.terminal[key] = true
	buffered := append([]Event(nil), m.buffers[key]...)
	delete(m.buffers, key)
	m.mu.Unlock()

	for _, ev := range buffered {
		if err := m.send(ev); err != nil {
			return err
		}
	}
	return nil
}

func (m *roundStreamMux) cancelBranch(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.terminal[key] = true
	delete(m.buffers, key)
}

func (m *roundStreamMux) send(ev Event) error {
	select {
	case m.events <- ev:
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
	return nil
}

func contextIDFromAttrs(attrs map[string]string) string {
	return attrs[telemetry.AttrConversationID]
}

type roundController struct {
	claw        *Claw
	contextID   string
	userText    string
	cancel      context.CancelFunc
	interruptCh chan engine.Interrupt

	mu              sync.Mutex
	done            bool
	interrupted     bool
	discard         bool
	partial         strings.Builder
	partialSnapshot string
	partialSaved    bool
}

func newRoundController(c *Claw, contextID, userText string, cancel context.CancelFunc) *roundController {
	return &roundController{
		claw:        c,
		contextID:   contextID,
		userText:    userText,
		cancel:      cancel,
		interruptCh: make(chan engine.Interrupt, 1),
		discard:     true,
	}
}

func (r *roundController) interrupts() <-chan engine.Interrupt {
	return r.interruptCh
}

func (r *roundController) recordRead(ev Event) {
	if ev.Type != EventToken || ev.Content == "" {
		return
	}
	r.mu.Lock()
	if !r.interrupted {
		r.partial.WriteString(ev.Content)
	}
	r.mu.Unlock()
}

func (r *roundController) interrupt(discard bool) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.interrupted = true
	if !discard {
		r.discard = false
		r.partialSnapshot = r.partial.String()
	}
	cancel := r.cancel
	ch := r.interruptCh
	r.mu.Unlock()

	select {
	case ch <- engine.Interrupt{Cause: engine.CauseUserCancel, Detail: "claw response interrupted"}:
	default:
	}
	cancel()
}

func (r *roundController) shouldCommitPartial() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupted && !r.discard && !r.partialSaved && r.partialSnapshot != ""
}

func (r *roundController) commitPartial(ctx context.Context) error {
	r.mu.Lock()
	if r.partialSaved || r.discard || r.partialSnapshot == "" {
		r.mu.Unlock()
		return nil
	}
	text := r.partialSnapshot
	r.partialSaved = true
	r.mu.Unlock()

	if r.claw == nil || r.claw.memory == nil {
		return nil
	}
	return r.claw.memory.saveTurn(context.WithoutCancel(ctx), r.contextID, r.userText, model.NewTextMessage(model.RoleAssistant, text), nil)
}

func (r *roundController) finish() {
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
}
