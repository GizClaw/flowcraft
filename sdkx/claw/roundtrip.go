package claw

import (
	"context"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
)

// EventType is a public Claw stream event discriminator.
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
	Type       EventType     `json:"type"`
	Content    string        `json:"content,omitempty"`
	ID         string        `json:"id,omitempty"`
	Name       string        `json:"name,omitempty"`
	Arguments  any           `json:"arguments,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	IsError    bool          `json:"is_error,omitempty"`
	Result     *agent.Result `json:"result,omitempty"`
	Err        string        `json:"error,omitempty"`
}

// Response is a blocking iterator over one round trip's events.
type Response struct {
	events <-chan Event
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
	return ev, nil
}

// RoundTrip starts one user turn for context id. Only one active turn per id is allowed.
func (c *Claw) RoundTrip(id string, req Request) (*Response, error) {
	if c == nil {
		return nil, errdefs.Validationf("claw: nil Claw")
	}
	if id == "" {
		id = "default"
	}
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	if _, ok := c.active[id]; ok {
		c.mu.Unlock()
		return nil, errdefs.Conflictf("claw: context %q already has an active round trip", id)
	}
	c.active[id] = struct{}{}
	c.mu.Unlock()

	events := make(chan Event, 32)
	resp := &Response{events: events}
	go c.runRoundTrip(ctx, id, req, events)
	return resp, nil
}

func (c *Claw) runRoundTrip(ctx context.Context, id string, req Request, events chan<- Event) {
	defer func() {
		c.mu.Lock()
		delete(c.active, id)
		c.mu.Unlock()
		close(events)
	}()

	host := engine.HostFuncs{
		Inner: engine.NoopHost{},
		PublishFn: func(_ context.Context, env event.Envelope) error {
			ev, ok, err := eventFromEnvelope(env)
			if err != nil {
				select {
				case events <- Event{Type: EventError, Err: err.Error(), IsError: true}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			}
			if !ok {
				return nil
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		},
	}
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
		events <- Event{Type: EventError, Err: result.Err.Error(), IsError: true, Result: result}
		return
	}
	if c.memory != nil {
		if err := c.memory.saveTurn(ctx, id, req.Text, latestAssistant(result.Messages)); err != nil {
			events <- Event{Type: EventError, Err: err.Error(), IsError: true, Result: result}
			return
		}
	}
	events <- Event{Type: EventResult, Result: result}
}

func (c *Claw) boardSeeder() agent.BoardSeeder {
	return agent.BoardSeederFunc(func(ctx context.Context, _ agent.RunInfo, req *agent.Request) (*engine.Board, error) {
		board := engine.NewBoard()
		if c.memory != nil {
			memText, err := c.memory.recallContext(ctx, req.Message.Content())
			if err != nil {
				return nil, fmt.Errorf("claw: recall memory: %w", err)
			}
			if memText != "" {
				board.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleSystem, memText))
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
		Type:       EventType(delta.Type),
		Content:    delta.Content,
		ID:         delta.ID,
		Name:       delta.Name,
		Arguments:  delta.Arguments,
		ToolCallID: delta.ToolCallID,
		IsError:    delta.IsError,
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

func contextIDFromAttrs(attrs map[string]string) string {
	return attrs[telemetry.AttrConversationID]
}
