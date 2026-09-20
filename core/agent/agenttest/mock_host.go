package agenttest

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// MockHost is a fully-featured [agent.Host] for tests. It records
// every interaction and lets the test inject cooperative interrupts
// and user replies.
//
// It also implements the optional [agent.SteerSource] capability, so
// engine suites can drive host.drainSteer() the way a deployment's
// steer node would: pre-load messages with [MockHost.Steer], then
// assert what the engine did with them.
//
// All methods are safe for concurrent use; any number of goroutines
// inside the engine may call them while the test inspects state.
//
// Zero value is NOT ready to use. Call [NewMockHost] which
// pre-allocates the channels.
type MockHost struct {
	interruptCh chan agent.Interrupt

	mu          sync.Mutex
	envelopes   []event.Envelope
	usages      []inference.Usage
	checkpoints []agent.Checkpoint
	prompts     []agent.UserPrompt
	steered     []message.Message

	// reply is what AskUser returns when invoked. nil means "return a
	// NotAvailable error" so engines can verify they propagate it.
	reply *agent.UserReply

	// publishErr, if non-nil, is returned from every Publish to let
	// tests assert engines tolerate observability failures.
	publishErr error

	// checkpointErr, if non-nil, is returned from every Checkpoint.
	checkpointErr error

	// usageErr, if non-nil, is returned from every ReportUsage call
	// (after the usage value is recorded).
	usageErr error
}

// NewMockHost returns a ready-to-use MockHost. The interrupt channel
// is buffered so [MockHost.Interrupt] never blocks the test goroutine
// when the engine has not yet reached its select on Interrupts().
func NewMockHost() *MockHost {
	return &MockHost{
		interruptCh: make(chan agent.Interrupt, 1),
	}
}

// ---------- agent.Publisher ----------

// Publish records the envelope and returns the configured publishErr
// (default nil).
func (h *MockHost) Publish(_ context.Context, env event.Envelope) error {
	h.mu.Lock()
	h.envelopes = append(h.envelopes, env)
	err := h.publishErr
	h.mu.Unlock()
	return err
}

// SetPublishError configures all subsequent Publish calls to return
// err. Pass nil to clear.
func (h *MockHost) SetPublishError(err error) {
	h.mu.Lock()
	h.publishErr = err
	h.mu.Unlock()
}

// Envelopes returns a copy of every envelope received so far.
func (h *MockHost) Envelopes() []event.Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]event.Envelope, len(h.envelopes))
	copy(out, h.envelopes)
	return out
}

// ---------- agent.Interrupter ----------

// Interrupts returns the cooperative-interrupt channel.
func (h *MockHost) Interrupts() <-chan agent.Interrupt { return h.interruptCh }

// Interrupt queues an interrupt for the agent. Non-blocking: if the
// buffer is full the call drops the new signal silently — tests that
// care should call only once or assert via [MockHost.Envelopes] that
// the engine has reached its select.
func (h *MockHost) Interrupt(cause agent.Cause, detail string) {
	select {
	case h.interruptCh <- agent.Interrupt{Cause: cause, Detail: detail}:
	default:
	}
}

// ---------- agent.UserPrompter ----------

// AskUser records the prompt and returns the configured reply, or a
// NotAvailable error when no reply has been set.
func (h *MockHost) AskUser(_ context.Context, p agent.UserPrompt) (agent.UserReply, error) {
	h.mu.Lock()
	h.prompts = append(h.prompts, p)
	reply := h.reply
	h.mu.Unlock()
	if reply == nil {
		return agent.UserReply{}, errdefs.NotAvailablef("enginetest: no user reply configured")
	}
	return *reply, nil
}

// SetUserReply configures what AskUser returns. Pass nil to revert to
// the NotAvailable default.
func (h *MockHost) SetUserReply(reply *agent.UserReply) {
	h.mu.Lock()
	h.reply = reply
	h.mu.Unlock()
}

// Prompts returns a copy of every UserPrompt the engine submitted.
func (h *MockHost) Prompts() []agent.UserPrompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]agent.UserPrompt, len(h.prompts))
	copy(out, h.prompts)
	return out
}

// ---------- agent.Checkpointer ----------

// Checkpoint records cp and returns the configured checkpointErr.
func (h *MockHost) Checkpoint(_ context.Context, cp agent.Checkpoint) error {
	h.mu.Lock()
	h.checkpoints = append(h.checkpoints, cp)
	err := h.checkpointErr
	h.mu.Unlock()
	return err
}

// SetCheckpointError configures all subsequent Checkpoint calls to
// return err. Pass nil to clear.
func (h *MockHost) SetCheckpointError(err error) {
	h.mu.Lock()
	h.checkpointErr = err
	h.mu.Unlock()
}

// Checkpoints returns a copy of every checkpoint the engine submitted.
func (h *MockHost) Checkpoints() []agent.Checkpoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]agent.Checkpoint, len(h.checkpoints))
	copy(out, h.checkpoints)
	return out
}

// ---------- agent.UsageReporter ----------

// ReportUsage records the usage delta. Multiple calls are kept in
// order so tests can assert per-call totals; sum them with
// [MockHost.TotalUsage] when only the total matters.
func (h *MockHost) ReportUsage(_ context.Context, usage inference.Usage) error {
	h.mu.Lock()
	h.usages = append(h.usages, usage)
	err := h.usageErr
	h.mu.Unlock()
	return err
}

// SetUsageError configures all subsequent ReportUsage calls to return
// err (typically errdefs.BudgetExceeded). Pass nil to clear.
func (h *MockHost) SetUsageError(err error) {
	h.mu.Lock()
	h.usageErr = err
	h.mu.Unlock()
}

// Usages returns a copy of every usage report.
func (h *MockHost) Usages() []inference.Usage {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]inference.Usage, len(h.usages))
	copy(out, h.usages)
	return out
}

// TotalUsage sums every recorded usage report.
func (h *MockHost) TotalUsage() inference.Usage {
	h.mu.Lock()
	defer h.mu.Unlock()
	var sum inference.Usage
	for _, u := range h.usages {
		sum = sum.Add(u)
	}
	return sum
}

// ---------- agent.SteerSource ----------

// Steer queues msg the way a running turn receives it from Turn.Steer.
// It never rejects: MockHost models delivery to the engine — admission
// control (bounds, terminal state, size limits) belongs to the real
// turn-owned queue and is tested there.
func (h *MockHost) Steer(msg message.Message) {
	h.mu.Lock()
	h.steered = append(h.steered, msg.Clone())
	h.mu.Unlock()
}

// DrainSteer implements [agent.SteerSource]: it returns clones of
// everything queued and empties the queue, the way the real
// turn-owned queue does — the mock never hands out a message it still
// holds.
func (h *MockHost) DrainSteer() []message.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.steered) == 0 {
		return nil
	}
	out := make([]message.Message, len(h.steered))
	for i, msg := range h.steered {
		out[i] = msg.Clone()
	}
	h.steered = nil
	return out
}

// SteeredMessages returns clones of the messages still queued, without
// draining them, so a test can assert delivery position and let the
// engine drain afterwards. Cloning keeps a caller that mutates the
// returned messages from changing what the next drain delivers.
func (h *MockHost) SteeredMessages() []message.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]message.Message, len(h.steered))
	for i, msg := range h.steered {
		out[i] = msg.Clone()
	}
	return out
}

// Compile-time assertion that MockHost satisfies agent.Host.
var _ agent.Host = (*MockHost)(nil)

// ... and the optional steer capability engine suites assert against.
var _ agent.SteerSource = (*MockHost)(nil)
