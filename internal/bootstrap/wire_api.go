package bootstrap

import (
	"github.com/GizClaw/flowcraft/internal/api"
	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// WireEventsHandler builds the HTTP-pull / readyz handler. The readyer
// argument may be nil during early local development; production wiring
// supplies a ProjectorManager-backed adapter.
func WireEventsHandler(log *eventlog.SQLiteLog, pol policy.Policy, mgr *projection.Manager) *api.EventsHandler {
	if mgr == nil {
		return api.NewEventsHandler(log, pol, nil)
	}
	adapter := &api.ReadinessAdapter{
		IsAllReadyFunc: mgr.IsAllReady,
		StatusFunc: func() []api.ProjectorStatusView {
			st := mgr.Status()
			out := make([]api.ProjectorStatusView, 0, len(st))
			for _, s := range st {
				out = append(out, api.ProjectorStatusView{
					Name:                s.Name,
					CheckpointSeq:       s.CheckpointSeq,
					LatestSeq:           s.LatestSeq,
					Lag:                 s.Lag,
					Ready:               s.Ready,
					ConsecutiveFailures: s.ConsecutiveFailures,
					LastError:           s.LastError,
				})
			}
			return out
		},
	}
	return api.NewEventsHandler(log, pol, adapter)
}
