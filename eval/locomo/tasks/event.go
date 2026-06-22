package tasks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

func RunEvent(ctx context.Context, mem *memory.System, answer llm.LLM, scope memory.Scope, target dataset.EventSummary, timeout time.Duration) locomoreport.EventResult {
	gold := strings.Join(target.Events, "\n")
	row := locomoreport.EventResult{SessionIndex: target.SessionIndex, Speaker: target.Speaker, Gold: gold}
	query := fmt.Sprintf("Summarize important events for session %d", target.SessionIndex)
	if target.Speaker != "" {
		query += " and speaker " + target.Speaker
	}
	pack, err := mem.PackContext(ctx, memory.ContextRequest{Scope: scope, Query: query, TopK: 8})
	if err != nil {
		row.Error = err.Error()
		return row
	}
	pred, err := generateTextMessages(ctx, answer, eventSummaryMessages(pack, query), timeout)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.Predicted = pred
	row.TokenF1 = tokenF1(pred, gold)
	row.Rouge1 = rouge1(pred, gold)
	row.RougeL = rougeL(pred, gold)
	return row
}

func EventsForSession(events []dataset.EventSummary, session int) []dataset.EventSummary {
	var out []dataset.EventSummary
	for _, event := range events {
		if event.SessionIndex == session {
			out = append(out, event)
		}
	}
	return out
}
