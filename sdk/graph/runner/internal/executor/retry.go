package executor

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
)

// executeWithRetry runs node up to maxAttempts times, restoring the board
// snapshot before each retry. The publisher passed to the node is wrapped so
// that tool_call / tool_result events emitted by the node are mirrored into
// board state (legacy contract preserved for the LLM tool loop).
func executeWithRetry(ctx context.Context, node graph.Node, board *graph.Board, cfg runConfig, nodeID string) error {
	maxAttempts := 1 + cfg.maxNodeRetries
	var lastErr error

	publisher := newNodePublisher(ctx, cfg, nodeID)
	wrappedPublisher := wrapToolCapture(publisher, board)

	for attempt := range maxAttempts {
		if attempt > 0 {
			backoff := time.Duration(attempt) * 500 * time.Millisecond
			select {
			case <-ctx.Done():
				return errdefs.FromContext(ctx.Err())
			case <-time.After(backoff):
			}
		}

		snapshot := board.Snapshot()
		execCtx := graph.ExecutionContext{
			Context:    ctx,
			Host:       cfg.host,
			Publisher:  wrappedPublisher,
			RunID:      cfg.runID,
			Deps:       cfg.deps,
			Attributes: cfg.attributes,
		}

		lastErr = node.ExecuteBoard(execCtx, board)
		if lastErr == nil {
			return nil
		}
		// Cooperative interrupts (engine.Interrupted) are never retried:
		// the node returned with intent, not failure, so re-running would
		// either burn budget on a duplicate side effect or simply trip
		// the same interrupt again.
		if errdefs.IsInterrupted(lastErr) {
			return lastErr
		}

		if attempt < maxAttempts-1 {
			board.RestoreFrom(snapshot)
		}
	}
	return lastErr
}

// wrapToolCapture returns a publisher that forwards every event to inner and
// additionally mirrors tool_call / tool_result events into the board's
// __tool_calls slice. The mirror lives outside the LLM node so other emitters
// (e.g. script nodes invoking tools) participate in the same state machine.
func wrapToolCapture(inner graph.StreamPublisher, board *graph.Board) graph.StreamPublisher {
	return graph.StreamPublisherFunc(func(eventType string, payload any) {
		inner.Emit(eventType, payload)
		m, ok := payload.(map[string]any)
		if !ok {
			return
		}
		switch eventType {
		case "tool_call":
			_ = board.AppendSliceVar(graph.VarToolCalls, map[string]any{
				"id":     m["id"],
				"name":   m["name"],
				"args":   m["arguments"],
				"status": "pending",
			})
		case "tool_result":
			updateBoardToolResult(board, m)
		}
	})
}

func updateBoardToolResult(board *graph.Board, m map[string]any) {
	tcID, _ := m["tool_call_id"].(string)
	if tcID == "" {
		return
	}
	result, _ := m["content"].(string)
	status := "success"
	if isErr, ok := m["is_error"].(bool); ok && isErr {
		status = "error"
	}

	board.UpdateSliceVarItem(graph.VarToolCalls, func(item any) bool {
		if entry, ok := item.(map[string]any); ok {
			return entry["id"] == tcID
		}
		return false
	}, func(item any) any {
		if entry, ok := item.(map[string]any); ok {
			entry["result"] = result
			entry["status"] = status
		}
		return item
	})
}
