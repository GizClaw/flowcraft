package graph

import (
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// MessageStream accumulates a streaming assistant message for one
// board channel: each Emit publishes a token stream-delta in real
// time, and Close appends the accumulated text as a single message.
//
// It exists for message-producing node types (typical: LLM nodes
// consuming a streaming provider): the board stays clean — one
// message per response, not one per token — while observers still see
// output as it happens.
//
//	s := ec.NewMessageStream(cfg.MessagesChannel)
//	for token := range providerStream {
//	    if err := s.Emit(token); err != nil { return err }
//	}
//	return s.Close()
type MessageStream struct {
	ec      ExecutionContext
	channel string
	mu      sync.Mutex
	buf     strings.Builder

	materialized bool
	message      message.Message
}

// NewMessageStream starts a stream bound to channel. An empty channel
// means the main channel (agent.MainChannel).
func (ec ExecutionContext) NewMessageStream(channel string) *MessageStream {
	if channel == "" {
		channel = agent.MainChannel
	}
	return &MessageStream{ec: ec, channel: channel}
}

// Emit publishes one text increment as a token stream-delta and buffers
// it for Close.
func (s *MessageStream) Emit(token string) error {
	s.mu.Lock()
	s.buf.WriteString(token)
	s.mu.Unlock()
	return s.ec.EmitStreamDelta(agent.StreamDeltaPayload{
		Type:    agent.StreamDeltaToken,
		Content: token,
	})
}

// Close appends the accumulated text as one assistant message to the
// bound channel and returns the message. Closing an empty stream is a
// no-op returning an empty message and nil error.
func (s *MessageStream) Close(board *agent.Board) (message.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.materialized {
		return s.message, nil
	}
	s.materialized = true
	text := s.buf.String()
	if text == "" {
		return message.Message{}, nil
	}
	msg := message.NewTextMessage(message.RoleAssistant, text)
	board.AppendChannelMessage(s.channel, msg)
	s.message = msg
	return msg, nil
}

// Channel returns the board channel the stream is bound to.
func (s *MessageStream) Channel() string { return s.channel }
