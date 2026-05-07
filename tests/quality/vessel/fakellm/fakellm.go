// Package fakellm is a self-contained, scriptable llm.LLM
// implementation for vessel quality and e2e tests. It is
// deliberately separate from sdkx/llm/mock — that package is a
// global side-effect provider wired into vesseld's builtin
// catalog and tuned for legacy E2E flows; we want a fresh test
// utility we can extend with chaos-injection knobs without
// destabilising production wiring.
//
// # Scripted behaviour
//
// Each fake instance owns a queue of [Step]s. Generate consumes
// the head step on every call:
//
//	steps := []fakellm.Step{
//	    {Text: "hello"},
//	    {ToolCall: &fakellm.Tool{Name: "search", Args: `{"q":"x"}`}},
//	    {Err: errors.New("provider 503")},
//	}
//	llm := fakellm.New(steps...)
//
// When the queue empties the fake either repeats the last step
// (when [WithRepeatLast] is enabled) or returns
// [ErrScriptExhausted] so tests fail loudly on unintended
// extra calls instead of silently looping.
//
// # Recording
//
// Every Generate call appends a [Call] entry — captured
// messages, tools, options — to an internal slice. Tests inspect
// them via [LLM.Calls] for invariants like "the second turn saw
// the tool result" or "system prompt was prepended".
package fakellm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// ErrScriptExhausted is returned by Generate when the script is
// drained and [WithRepeatLast] is not set. We surface a sentinel
// error so callers can detect "the test wired N replies but the
// agent looped past N" without string matching.
var ErrScriptExhausted = errors.New("fakellm: script exhausted (no more steps configured)")

// Tool describes a tool_call reply line.
//
// Args is the raw JSON string the LLM would return; tests can
// hand-craft it (`{"q":"x"}`) or build via [llm.MarshalToolArgs].
// ID defaults to "call_<index>" when empty so tests do not have
// to invent one.
type Tool struct {
	ID   string
	Name string
	Args string
}

// Step is one scripted turn. Exactly one of Text / ToolCalls /
// Err should be set; if Text and ToolCalls are both set the
// reply contains both (rare but legal in the OpenAI protocol).
type Step struct {
	// Text is the plain assistant content. Empty + no ToolCalls
	// means the fake returns an empty reply (which the calling
	// engine will treat as "done").
	Text string

	// ToolCalls, when non-empty, makes the assistant message a
	// tool_call message. Multiple entries trigger parallel tool
	// dispatch when the engine supports it.
	ToolCalls []Tool

	// Err, when non-nil, makes Generate return (zero, zero, Err).
	// Use sentinel errors from sdk/errdefs (e.g.
	// errdefs.RateLimitedf) to assert vessel error
	// classification.
	Err error

	// Delay, when non-zero, sleeps for the duration before
	// returning. Honours ctx cancellation: if ctx fires first,
	// Generate returns ctx.Err() instead of executing the step.
	Delay time.Duration

	// Usage overrides the default TokenUsage. Zero value yields
	// {Input:5, Output:5, Total:10} which is plausible enough to
	// not trip "missing usage" guards.
	Usage *llm.TokenUsage
}

// Call captures one Generate invocation for later inspection.
type Call struct {
	StepIndex int
	Messages  []llm.Message
	Options   *llm.GenerateOptions
}

// LLM is a scripted llm.LLM. The zero value is unusable; build
// with [New].
type LLM struct {
	mu          sync.Mutex
	steps       []Step
	idx         int
	calls       []Call
	repeatLast  bool
	defaultUsg  llm.TokenUsage
	streamWords int
}

// Option mutates an [LLM] at construction.
type Option func(*LLM)

// WithRepeatLast makes Generate repeat the final step instead of
// returning ErrScriptExhausted once the script empties. Useful
// for "agent loops three times until it decides to stop" scenarios
// where exact length is irrelevant.
func WithRepeatLast() Option { return func(m *LLM) { m.repeatLast = true } }

// WithDefaultUsage overrides the per-step default TokenUsage.
func WithDefaultUsage(u llm.TokenUsage) Option {
	return func(m *LLM) { m.defaultUsg = u }
}

// WithStreamChunkSize controls how many words go into each
// StreamMessage chunk; default is 3. Set to 1 to exercise per-
// token routing in stream observers.
func WithStreamChunkSize(n int) Option {
	return func(m *LLM) {
		if n > 0 {
			m.streamWords = n
		}
	}
}

// New builds a scripted LLM with the supplied steps.
func New(steps []Step, opts ...Option) *LLM {
	m := &LLM{
		steps:       append([]Step(nil), steps...),
		defaultUsg:  llm.TokenUsage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
		streamWords: 3,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Push appends additional steps. Safe to call from a test
// goroutine concurrent with Generate.
func (m *LLM) Push(steps ...Step) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, steps...)
}

// Calls snapshots the recorded call slice. The returned slice is
// safe to mutate; the underlying buffer is copied.
func (m *LLM) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Call, len(m.calls))
	copy(out, m.calls)
	return out
}

// Reset clears recorded calls and rewinds the script index. The
// step list is preserved so tests can re-use a fixture across
// table-driven cases.
func (m *LLM) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idx = 0
	m.calls = nil
}

// Generate satisfies llm.LLM.
func (m *LLM) Generate(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	step, idx, err := m.advance()
	if err != nil {
		return llm.Message{}, llm.TokenUsage{}, err
	}
	m.record(idx, messages, llm.ApplyOptions(opts...))

	if step.Delay > 0 {
		select {
		case <-ctx.Done():
			return llm.Message{}, llm.TokenUsage{}, ctx.Err()
		case <-time.After(step.Delay):
		}
	}
	if step.Err != nil {
		return llm.Message{}, llm.TokenUsage{}, step.Err
	}

	usage := m.defaultUsg
	if step.Usage != nil {
		usage = *step.Usage
	}
	return m.buildMessage(step, idx), usage, nil
}

// GenerateStream satisfies llm.LLM by chunking Generate output.
func (m *LLM) GenerateStream(ctx context.Context, messages []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	msg, usage, err := m.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	return newStream(msg, usage, m.streamWords), nil
}

func (m *LLM) advance() (Step, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.steps) {
		if !m.repeatLast || len(m.steps) == 0 {
			return Step{}, m.idx, ErrScriptExhausted
		}
		return m.steps[len(m.steps)-1], len(m.steps) - 1, nil
	}
	s := m.steps[m.idx]
	cur := m.idx
	m.idx++
	return s, cur, nil
}

func (m *LLM) record(idx int, messages []llm.Message, opts *llm.GenerateOptions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]llm.Message, len(messages))
	copy(cp, messages)
	m.calls = append(m.calls, Call{StepIndex: idx, Messages: cp, Options: opts})
}

func (m *LLM) buildMessage(step Step, idx int) llm.Message {
	if len(step.ToolCalls) == 0 {
		return llm.NewTextMessage(llm.RoleAssistant, step.Text)
	}
	calls := make([]llm.ToolCall, 0, len(step.ToolCalls))
	for i, t := range step.ToolCalls {
		id := t.ID
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", idx, i)
		}
		calls = append(calls, llm.ToolCall{ID: id, Name: t.Name, Arguments: t.Args})
	}
	msg := llm.NewToolCallMessage(calls)
	if step.Text != "" {
		// Prepend a text part so observers see the assistant's
		// pre-tool-call narration. Order matters: callers tend
		// to render the text before invoking the tool.
		msg.Parts = append([]llm.Part{{Type: llm.PartText, Text: step.Text}}, msg.Parts...)
	}
	return msg
}
