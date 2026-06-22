package tasks

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

func RunDialog(ctx context.Context, mem *memory.System, answer llm.LLM, scope memory.Scope, c dataset.DialogCase, timeout time.Duration) locomoreport.DialogResult {
	row := locomoreport.DialogResult{ID: c.ID, Caption: c.Caption, Query: c.Query, Gold: c.Gold}
	query := strings.TrimSpace(c.Query)
	if query == "" {
		query = "Generate the next dialog response for this image caption."
	}
	pack, err := mem.PackContext(ctx, memory.ContextRequest{Scope: scope, Query: query + " " + c.Caption, TopK: 8})
	if err != nil {
		row.Error = err.Error()
		return row
	}
	pred, err := generateTextMessages(ctx, answer, dialogAnswerMessages(pack, c), timeout)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.Predicted = pred
	row.BleuLite = bleuLite(pred, c.Gold)
	row.RougeL = rougeL(pred, c.Gold)
	row.CaptionTermRecall = captionTermRecall(pred, c.Caption)
	return row
}

func DialogCaseForTurn(cases []dataset.DialogCase, session int, diaID string) (dataset.DialogCase, bool) {
	for _, c := range cases {
		if c.SessionIndex == session && c.SourceTurnDiaID == diaID {
			return c, true
		}
	}
	return dataset.DialogCase{}, false
}
