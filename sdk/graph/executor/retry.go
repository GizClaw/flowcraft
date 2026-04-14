package executor

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	nodevar "github.com/GizClaw/flowcraft/sdk/graph/node"
)

func executeWithRetry(ctx context.Context, node graph.Node, board *graph.Board, cfg runConfig) error {
	maxAttempts := 1 + cfg.maxNodeRetries
	var lastErr error

	streamCb := cfg.streamCallback
	wrappedCb := func(se graph.StreamEvent) {
		if streamCb != nil {
			streamCb(se)
		}
		if m, ok := se.Payload.(map[string]any); ok {
			switch se.Type {
			case "tool_call":
				_ = board.AppendSliceVar(nodevar.VarToolCalls, map[string]any{
					"id":     m["id"],
					"name":   m["name"],
					"args":   m["arguments"],
					"status": "pending",
				})
			case "tool_result":
				updateBoardToolResult(board, m)
			}
		}
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * 500 * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		snapshot := board.Snapshot()
		execCtx := graph.ExecutionContext{
			Context: ctx,
			Stream:  wrappedCb,
			RunID:   cfg.runID,
		}

		lastErr = node.ExecuteBoard(execCtx, board)
		if lastErr == nil {
			return nil
		}
		if errdefs.Is(lastErr, graph.ErrInterrupt) {
			return lastErr
		}

		if attempt < maxAttempts-1 {
			board.RestoreFrom(snapshot)
		}
	}
	return lastErr
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

	board.UpdateSliceVarItem(nodevar.VarToolCalls, func(item any) bool {
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
