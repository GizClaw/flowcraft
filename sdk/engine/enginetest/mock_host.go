package enginetest

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// MockHost is a fully-featured [engine.Host] for tests. It records
// every interaction and lets the test inject cooperative interrupts
// and user replies.
//
// All methods are safe for concurrent use; any number of goroutines
// inside the engine may call them while the test inspects state.
//
// Zero value is NOT ready to use. Call [NewMockHost] which
// pre-allocates the channels.
type MockHost struct {
	interruptCh chan engine.Interrupt

	mu          sync.Mutex
	envelopes   []event.Envelope
	usages      []model.TokenUsage
	checkpoints []engine.Checkpoint
	prompts     []engine.UserPrompt

	// reply is what AskUser returns when invoked. nil means "return a
	// NotAvailable error" so engines can verify they propagate it.
	reply *engine.UserReply

	// publishErr, if non-nil, is returned from every Publish to let
	// tests assert engines tolerate observability failures.
	publishErr error

	// checkpointErr, if non-nil, is returned from every Checkpoint.
	checkpointErr error
}

// NewMockHost returns a ready-to-use MockHost. The interrupt channel
// is buffered so [MockHost.Interrupt] never blocks the test goroutine
// when the engine has not yet reached its select on Interrupts().
func NewMockHost() *MockHost {
	return &MockHost{
		interruptCh: make(chan engine.Interrupt, 1),
	}
}

// ---------- engine.Publisher ----------

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

// ---------- engine.Interrupter ----------

// Interrupts returns the cooperative-interrupt channel.
func (h *MockHost) Interrupts() <-chan engine.Interrupt { return h.interruptCh }

// Interrupt queues an interrupt for the engine. Non-blocking: if the
// buffer is full the call drops the new signal silently — tests that
// care should call only once or assert via [MockHost.Envelopes] that
// the engine has reached its select.
func (h *MockHost) Interrupt(cause engine.Cause, detail string) {
	select {
	case h.interruptCh <- engine.Interrupt{Cause: cause, Detail: detail}:
	default:
	}
}

// ---------- engine.UserPrompter ----------

// AskUser records the prompt and returns the configured reply, or a
// NotAvailable error when no reply has been set.
func (h *MockHost) AskUser(_ context.Context, p engine.UserPrompt) (engine.UserReply, error) {
	h.mu.Lock()
	h.prompts = append(h.prompts, p)
	reply := h.reply
	h.mu.Unlock()
	if reply == nil {
		return engine.UserReply{}, errdefs.NotAvailablef("enginetest: no user reply configured")
	}
	return *reply, nil
}

// SetUserReply configures what AskUser returns. Pass nil to revert to
// the NotAvailable default.
func (h *MockHost) SetUserReply(reply *engine.UserReply) {
	h.mu.Lock()
	h.reply = reply
	h.mu.Unlock()
}

// Prompts returns a copy of every UserPrompt the engine submitted.
func (h *MockHost) Prompts() []engine.UserPrompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]engine.UserPrompt, len(h.prompts))
	copy(out, h.prompts)
	return out
}

// ---------- engine.Checkpointer ----------

// Checkpoint records cp and returns the configured checkpointErr.
func (h *MockHost) Checkpoint(_ context.Context, cp engine.Checkpoint) error {
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
func (h *MockHost) Checkpoints() []engine.Checkpoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]engine.Checkpoint, len(h.checkpoints))
	copy(out, h.checkpoints)
	return out
}

// ---------- engine.UsageReporter ----------

// ReportUsage records the usage delta. Multiple calls are kept in
// order so tests can assert per-call totals; sum them with
// [MockHost.TotalUsage] when only the total matters.
func (h *MockHost) ReportUsage(_ context.Context, usage model.TokenUsage) error {
	h.mu.Lock()
	h.usages = append(h.usages, usage)
	h.mu.Unlock()
	return nil
}

// Usages returns a copy of every usage report.
func (h *MockHost) Usages() []model.TokenUsage {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]model.TokenUsage, len(h.usages))
	copy(out, h.usages)
	return out
}

// TotalUsage sums every recorded usage report.
func (h *MockHost) TotalUsage() model.TokenUsage {
	h.mu.Lock()
	defer h.mu.Unlock()
	var sum model.TokenUsage
	for _, u := range h.usages {
		sum = sum.Add(u)
	}
	return sum
}

// Compile-time assertion that MockHost satisfies engine.Host.
var _ engine.Host = (*MockHost)(nil)
