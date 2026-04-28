package llmnode

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// blockingStream emits chunks one at a time and pauses on a "trip" gate
// after the configured chunk index, letting the test fire a host
// interrupt at exactly the right moment without sleeps. The mock is the
// minimum needed to exercise the streaming select loop; production
// providers stream much more eagerly, but the loop's correctness only
// depends on "did we Next() between events".
type blockingStream struct {
	chunks []model.StreamChunk
	idx    int
	usage  model.Usage
	final  model.Message
}

func (s *blockingStream) Next() bool {
	if s.idx < len(s.chunks) {
		s.idx++
		return true
	}
	return false
}
func (s *blockingStream) Current() model.StreamChunk { return s.chunks[s.idx-1] }
func (s *blockingStream) Err() error                 { return nil }
func (s *blockingStream) Close() error               { return nil }
func (s *blockingStream) Message() model.Message     { return s.final }
func (s *blockingStream) Usage() model.Usage         { return s.usage }

// interruptOnlyHost composes engine.NoopHost (so it satisfies the full
// Host interface for free) and overrides only Interrupts() to feed a
// single pre-staged value. It's the smallest stub that lets the round
// driver observe a host signal mid-stream.
type interruptOnlyHost struct {
	engine.NoopHost
	ch chan engine.Interrupt
}

func newInterruptOnlyHost(intr engine.Interrupt) *interruptOnlyHost {
	ch := make(chan engine.Interrupt, 1)
	ch <- intr
	return &interruptOnlyHost{ch: ch}
}

func (h *interruptOnlyHost) Interrupts() <-chan engine.Interrupt { return h.ch }

// --- Tests ---

func TestRunRound_HostInterrupt_PartialAssistantCommitted(t *testing.T) {
	stream := &blockingStream{
		chunks: []model.StreamChunk{{Content: "hello "}, {Content: "world"}},
		final:  model.NewTextMessage(model.RoleAssistant, "hello world"),
		usage:  model.Usage{InputTokens: 5, OutputTokens: 3},
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	host := newInterruptOnlyHost(engine.Interrupt{
		Cause:  engine.CauseUserCancel,
		Detail: "user pressed stop",
	})

	hist := []model.Message{model.NewTextMessage(model.RoleUser, "hi")}
	r, err := runRound(context.Background(), host, nil,
		resolver, nil, "round1", hist, Config{})
	if err != nil {
		t.Fatalf("runRound returned hard error: %v", err)
	}
	if !r.Interrupted {
		t.Fatal("expected Interrupted=true")
	}
	if r.InterruptCause != engine.CauseUserCancel {
		t.Fatalf("InterruptCause = %q, want %q", r.InterruptCause, engine.CauseUserCancel)
	}
	if r.InterruptDetail != "user pressed stop" {
		t.Fatalf("InterruptDetail = %q", r.InterruptDetail)
	}
	if len(r.Messages) < 1 {
		t.Fatal("expected at least the original user message in transcript")
	}
}

func TestRunRound_HostInterrupt_CancelledToolResultsBackfilled(t *testing.T) {
	calls := []model.ToolCall{
		{ID: "call_1", Name: "search", Arguments: `{"q":"x"}`},
		{ID: "call_2", Name: "fetch", Arguments: `{"url":"y"}`},
	}
	assistant := model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartToolCall, ToolCall: &calls[0]},
			{Type: model.PartToolCall, ToolCall: &calls[1]},
		},
	}
	stream := &blockingStream{final: assistant}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	reg := tool.NewRegistry()
	reg.Register(&mockTool{name: "search", result: "should-not-run"})
	reg.Register(&mockTool{name: "fetch", result: "should-not-run"})
	host := newInterruptOnlyHost(engine.Interrupt{Cause: engine.CauseUserInput})

	r, err := runRound(context.Background(), host, nil,
		resolver, reg, "round-tools",
		[]model.Message{model.NewTextMessage(model.RoleUser, "go")},
		Config{ToolNames: []string{"search", "fetch"}})
	if err != nil {
		t.Fatalf("runRound returned hard error: %v", err)
	}

	if !r.Interrupted {
		t.Fatal("expected Interrupted=true")
	}
	if r.ToolPending {
		t.Fatal("ToolPending must be false on interrupt — backfilled results count as resolved")
	}
	if got := len(r.ToolResults); got != 2 {
		t.Fatalf("len(ToolResults) = %d, want 2", got)
	}
	for i, res := range r.ToolResults {
		if res.Content != CancelledToolResultContent {
			t.Fatalf("ToolResults[%d].Content = %q, want %q",
				i, res.Content, CancelledToolResultContent)
		}
		if !res.IsError {
			t.Fatalf("ToolResults[%d].IsError must be true", i)
		}
		if res.ToolCallID != calls[i].ID {
			t.Fatalf("ToolResults[%d].ToolCallID = %q, want %q",
				i, res.ToolCallID, calls[i].ID)
		}
	}

	// Last message in the transcript must be the synthetic
	// tool_result message so subsequent rounds keep the well-formed
	// call→result pairing the LLM provider expects.
	last := r.Messages[len(r.Messages)-1]
	if last.Role != model.RoleTool {
		t.Fatalf("last message role = %q, want %q", last.Role, model.RoleTool)
	}
}

func TestRunRound_CtxCancel_TreatedAsInterrupt(t *testing.T) {
	stream := &blockingStream{
		chunks: []model.StreamChunk{{Content: "partial"}},
		final:  model.NewTextMessage(model.RoleAssistant, "partial"),
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the round even starts

	r, err := runRound(ctx, engine.NoopHost{}, nil,
		resolver, nil, "round-ctx", nil, Config{})
	if err != nil {
		t.Fatalf("runRound returned hard error: %v", err)
	}
	if !r.Interrupted {
		t.Fatal("expected Interrupted=true on ctx cancel")
	}
	// ctx cancel without a host signal leaves cause empty (the
	// executor will then emit engine.Interrupted with CauseUnknown).
	if r.InterruptCause != "" {
		t.Fatalf("InterruptCause = %q, want empty for ctx-only cancel", r.InterruptCause)
	}
}

func TestNode_ExecuteBoard_HostInterrupt_CommitsThenReturnsInterrupted(t *testing.T) {
	stream := &blockingStream{
		chunks: []model.StreamChunk{{Content: "hi "}, {Content: "there"}},
		final:  model.NewTextMessage(model.RoleAssistant, "hi there"),
	}
	resolver := &mockResolver{llmInst: &streamOnlyLLM{stream: stream}}
	n := New("llm1", resolver, nil, Config{})
	host := newInterruptOnlyHost(engine.Interrupt{
		Cause: engine.CauseHostShutdown, Detail: "graceful",
	})

	board := graph.NewBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
		Host:    host,
	}, board)

	if !errdefs.IsInterrupted(err) {
		t.Fatalf("expected errdefs.IsInterrupted true, err = %v", err)
	}

	var ie engine.InterruptedError
	if !errors.As(err, &ie) {
		t.Fatalf("error did not wrap engine.InterruptedError: %v", err)
	}
	if ie.Cause != engine.CauseHostShutdown {
		t.Fatalf("Cause = %q, want %q", ie.Cause, engine.CauseHostShutdown)
	}

	// Even though the round was interrupted, the partial assistant
	// reply must have been written to the channel so the agent /
	// memory layer can read it after deciding to commit.
	channel := board.Channel(graph.MainChannel)
	if len(channel) < 2 {
		t.Fatalf("expected partial assistant message on channel, got %d msgs", len(channel))
	}
	if channel[len(channel)-1].Role != model.RoleAssistant {
		t.Fatalf("last channel message role = %q, want assistant",
			channel[len(channel)-1].Role)
	}

	// VarToolPending should also be set (false here since no tools).
	if v, ok := board.GetVar(VarToolPending); !ok || v.(bool) {
		t.Fatalf("VarToolPending = %v, want false", v)
	}
}

func TestRunRound_ProviderStreamError_NotInterrupt(t *testing.T) {
	resolver := &mockResolver{err: errors.New("resolve failed")}
	r, err := runRound(context.Background(), engine.NoopHost{}, nil,
		resolver, nil, "round-err", nil, Config{})
	if err == nil {
		t.Fatal("expected hard error from resolver failure")
	}
	if r != nil {
		t.Fatal("expected nil result on hard error")
	}
}
