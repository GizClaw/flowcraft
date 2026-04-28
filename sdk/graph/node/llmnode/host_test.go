package llmnode

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// usageRecorderHost embeds NoopHost so it satisfies engine.Host with
// minimum boilerplate; only ReportUsage is overridden to capture every
// delta the LLM node hands off.
type usageRecorderHost struct {
	engine.NoopHost

	mu     sync.Mutex
	deltas []model.TokenUsage
}

func (h *usageRecorderHost) ReportUsage(_ context.Context, u model.TokenUsage) {
	h.mu.Lock()
	h.deltas = append(h.deltas, u)
	h.mu.Unlock()
}

// TestNode_ReportsDeltaUsageToHost guarantees the contract documented
// on engine.Host.ReportUsage: each call adds delta usage. The node MUST
// hand off the round's own usage, NOT the running total it accumulates
// in board.VarInternalUsage — otherwise hosts that sum across rounds
// would double-count.
func TestNode_ReportsDeltaUsageToHost(t *testing.T) {
	host := &usageRecorderHost{}
	resolver := &mockResolver{llmInst: &mockLLM{
		msg:   model.NewTextMessage(model.RoleAssistant, "hi"),
		usage: model.Usage{InputTokens: 7, OutputTokens: 11},
	}}
	n := New("llm1", resolver, nil, Config{})

	board := graph.NewBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})
	// Pre-existing total on the board to prove the node reports the
	// delta, not the running total: if the wiring was wrong we'd see
	// 7+OldTotal here.
	board.SetVar(VarInternalUsage, model.TokenUsage{
		InputTokens: 100, OutputTokens: 200, TotalTokens: 300,
	})

	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
		Host:    host,
	}, board)
	if err != nil {
		t.Fatalf("ExecuteBoard error: %v", err)
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.deltas) != 1 {
		t.Fatalf("ReportUsage calls = %d, want 1", len(host.deltas))
	}
	got := host.deltas[0]
	// The mock streams a 0-chunk text and only fills usage on Stream.
	// Our wiring should pass result.Usage straight through — input 7
	// output 11 (total derived).
	if got.InputTokens != 7 || got.OutputTokens != 11 {
		t.Fatalf("delta = %+v, want In=7 Out=11", got)
	}

	// Sanity: the board still carries the running total
	// (existing 300 + new 18) so future rounds and persistence layers
	// keep working.
	if v, ok := board.GetVar(VarInternalUsage); !ok {
		t.Fatal("board missing VarInternalUsage")
	} else if u, ok := v.(model.TokenUsage); !ok || u.TotalTokens != 318 {
		t.Fatalf("VarInternalUsage = %+v, want total 318", v)
	}
}

// TestNode_DoesNotReportZeroUsage avoids spamming the host with empty
// envelopes when a round produced no token output (e.g. resolver error
// path returning early, or a tool-only round whose usage we couldn't
// recover). Hosts implementing budget enforcement reasonably expect
// ReportUsage to mean "real consumption happened".
func TestNode_DoesNotReportZeroUsage(t *testing.T) {
	host := &usageRecorderHost{}
	// streamOnlyLLM with a 0-usage stream — provider didn't bother
	// populating usage, our defaults stay zero.
	resolver := &mockResolver{llmInst: &streamOnlyLLM{
		stream: &blockingStream{
			final: model.NewTextMessage(model.RoleAssistant, ""),
		},
	}}
	n := New("llm1", resolver, nil, Config{})

	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
		Host:    host,
	}, graph.NewBoard())
	if err != nil {
		t.Fatalf("ExecuteBoard error: %v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.deltas) != 0 {
		t.Fatalf("expected zero ReportUsage calls, got %d (%+v)",
			len(host.deltas), host.deltas)
	}
}

// TestNode_NilHost_NoPanic guards the contract that Host is optional.
// In production every executor path provides one (NoopHost{} fallback),
// but the node itself must not panic if someone constructs an
// ExecutionContext by hand without a Host — common in tests today.
func TestNode_NilHost_NoPanic(t *testing.T) {
	resolver := &mockResolver{llmInst: &mockLLM{
		msg:   model.NewTextMessage(model.RoleAssistant, "hi"),
		usage: model.Usage{InputTokens: 1, OutputTokens: 1},
	}}
	n := New("llm1", resolver, nil, Config{})

	board := graph.NewBoard()
	board.SetChannel(graph.MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	err := n.ExecuteBoard(graph.ExecutionContext{
		Context: context.Background(),
		// Host intentionally nil
	}, board)
	if err != nil {
		t.Fatalf("ExecuteBoard error: %v", err)
	}
}
