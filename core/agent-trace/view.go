package agenttrace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// Calls returns every model call the task recorded, in order.
func (t *Trace) Calls() []agent.LLMCallRecord {
	var calls []agent.LLMCallRecord
	for _, fact := range t.Facts {
		if call, ok := fact.Value.(agent.LLMCallRecord); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

// Effects returns every tool execution the task recorded, in order.
func (t *Trace) Effects() []agent.EffectRecord {
	var effects []agent.EffectRecord
	for _, fact := range t.Facts {
		if effect, ok := fact.Value.(agent.EffectRecord); ok {
			effects = append(effects, effect)
		}
	}
	return effects
}

// StateWrites returns every state write, in order: the deltas a projection is
// folded from.
func (t *Trace) StateWrites() []graph.StateWrite {
	var writes []graph.StateWrite
	for _, fact := range t.Facts {
		if write, ok := fact.Value.(graph.StateWrite); ok {
			writes = append(writes, write)
		}
	}
	return writes
}

// Messages returns the messages a protocol adapter appended, in order.
//
// They are the task's own messages, not the graph's transcript: a graph keeps
// its transcript in a state key, which [Trace.State] exposes. A deployment
// exposed over A2A has both, and they answer different questions.
func (t *Trace) Messages() []message.Message {
	var messages []message.Message
	for _, fact := range t.Facts {
		if msg, ok := fact.Value.(message.Message); ok {
			messages = append(messages, msg)
		}
	}
	return messages
}

// Usage totals the accounting of every recorded call, by the same rule the run
// record uses.
func (t *Trace) Usage() usage.Usage {
	var total usage.Usage
	for _, call := range t.Calls() {
		agent.AddUsage(&total, call.Usage)
	}
	return total
}

// State folds the state writes into the state the task holds now.
//
// A value whose encoding this build does not know is skipped rather than
// guessed at: the write is still in [Trace.StateWrites] for a caller that wants
// to look.
func (t *Trace) State() map[string]graph.Value {
	state := make(map[string]graph.Value)
	for _, write := range t.StateWrites() {
		if value, err := graph.DecodeValue(write.Value); err == nil {
			state[write.Key] = value
		}
	}
	return state
}

// Transcript returns the messages the state key holds, for a graph that keeps
// its conversation there.
func (t *Trace) Transcript(key string) ([]message.Message, error) {
	value, ok := t.State()[key]
	if !ok {
		return nil, fmt.Errorf("agenttrace: the task holds no state key %q", key)
	}
	transcript, err := value.Messages()
	if err != nil {
		return nil, err
	}
	return transcript.Slice(), nil
}

// ErrNoResponse reports a recorded call whose answer was not kept: the
// deployment recorded the call without its bytes.
var ErrNoResponse = fmt.Errorf("agenttrace: the call recorded no response body")

// Response returns the answer of the i-th recorded call, resolving the blob the
// engine stored it in when it did.
func (t *Trace) Response(ctx context.Context, i int) (message.Message, error) {
	calls := t.Calls()
	if i < 0 || i >= len(calls) {
		return message.Message{}, fmt.Errorf(
			"agenttrace: call %d does not exist; the task recorded %d", i, len(calls))
	}
	body := calls[i].Response
	if len(body) == 0 && calls[i].ResponseRef != "" && t.store != nil {
		fetched, err := t.store.GetBlob(ctx, calls[i].ResponseRef)
		if err != nil {
			return message.Message{}, err
		}
		body = fetched
	}
	if len(body) == 0 {
		return message.Message{}, ErrNoResponse
	}
	var answer message.Message
	if err := json.Unmarshal(body, &answer); err != nil {
		return message.Message{}, fmt.Errorf("agenttrace: decode the recorded answer: %w", err)
	}
	return answer, nil
}
