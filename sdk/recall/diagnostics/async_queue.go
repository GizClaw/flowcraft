package diagnostics

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// AsyncSemanticQueueHealth is the operator-facing queue snapshot used
// by health dashboards (Phase F.1c).
type AsyncSemanticQueueHealth struct {
	Pending        int
	Leased         int
	ExpiredLeases  int
	Failed         int
	DeadLetter     int
	Completed      int
	CancelledTotal int
	Backlog        int
}

// DiagnoseAsyncSemanticQueue folds raw queue Stats into health fields.
// Backlog is Pending + Leased (work not yet terminal).
func DiagnoseAsyncSemanticQueue(stats port.AsyncSemanticStats) AsyncSemanticQueueHealth {
	return AsyncSemanticQueueHealth{
		Pending:        stats.Pending,
		Leased:         stats.Leased,
		ExpiredLeases:  stats.ExpiredLeases,
		Failed:         stats.Failed,
		DeadLetter:     stats.DeadLetter,
		Completed:      stats.Completed,
		CancelledTotal: stats.CancelledTotal,
		Backlog:        stats.Pending + stats.Leased,
	}
}
