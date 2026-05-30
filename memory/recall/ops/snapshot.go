package ops

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
)

// Snapshot is a metrics-friendly view of the latest operator state.
type Snapshot struct {
	Time      time.Time              `json:"time"`
	RuntimeID string                 `json:"runtime_id,omitempty"`
	Scope     recall.Scope           `json:"scope,omitempty"`
	Status    recall.ReadinessStatus `json:"status,omitempty"`

	SideEffects   QueueSnapshot `json:"side_effects,omitempty"`
	AsyncSemantic QueueSnapshot `json:"async_semantic,omitempty"`

	Drain     *DrainSnapshot     `json:"drain,omitempty"`
	Reconcile *ReconcileSnapshot `json:"reconcile,omitempty"`
	Error     string             `json:"error,omitempty"`
}

// QueueSnapshot is the stable queue shape for logs, metrics adapters, and
// dashboards. It deliberately avoids exposing backend-specific row details.
type QueueSnapshot struct {
	Pending        int `json:"pending,omitempty"`
	Leased         int `json:"leased,omitempty"`
	ExpiredLeases  int `json:"expired_leases,omitempty"`
	DeadLetter     int `json:"dead_letter,omitempty"`
	Backlog        int `json:"backlog,omitempty"`
	Completed      int `json:"completed,omitempty"`
	CancelledTotal int `json:"cancelled_total,omitempty"`
}

// DrainSnapshot summarizes worker progress.
type DrainSnapshot struct {
	Claimed    int           `json:"claimed,omitempty"`
	Completed  int           `json:"completed,omitempty"`
	Failed     int           `json:"failed,omitempty"`
	DeadLetter int           `json:"dead_letter,omitempty"`
	Recovered  int           `json:"recovered,omitempty"`
	Duration   time.Duration `json:"duration,omitempty"`
}

// ReconcileSnapshot summarizes namespace-wide repair.
type ReconcileSnapshot struct {
	Scopes  int `json:"scopes,omitempty"`
	Rebuilt int `json:"rebuilt,omitempty"`
	Expired int `json:"expired,omitempty"`
	Failed  int `json:"failed,omitempty"`
}

// Snapshot converts an Event into the stable metrics-friendly view.
func (e Event) Snapshot() Snapshot {
	s := Snapshot{
		Time:      e.Time,
		RuntimeID: e.RuntimeID,
		Scope:     e.Scope,
		Error:     e.Err,
	}
	if e.Drain != nil {
		s.Scope = e.Drain.Scope
		s.SideEffects = queueFromSideEffectProcess(e.Drain.SideEffects)
		s.AsyncSemantic = queueFromAsyncProcess(e.Drain.AsyncSemantic)
		s.Drain = &DrainSnapshot{
			Claimed:    e.Drain.SideEffects.Claimed + e.Drain.AsyncSemantic.Claimed,
			Completed:  e.Drain.SideEffects.Completed + e.Drain.AsyncSemantic.Completed,
			Failed:     e.Drain.SideEffects.Failed + e.Drain.AsyncSemantic.Failed,
			DeadLetter: e.Drain.SideEffects.DeadLetter,
			Recovered:  e.Drain.AsyncSemantic.Recovered,
			Duration:   e.Drain.Duration,
		}
	}
	if e.Readiness != nil {
		s.Scope = e.Readiness.Scope
		s.Status = e.Readiness.Status
		for _, check := range e.Readiness.Checks {
			switch check.Name {
			case "side_effect_outbox":
				s.SideEffects = queueFromReadiness(check)
			case "async_semantic_queue":
				s.AsyncSemantic = queueFromReadiness(check)
			}
		}
	}
	if e.Reconcile != nil {
		s.Reconcile = &ReconcileSnapshot{
			Scopes:  e.Reconcile.Scopes,
			Rebuilt: e.Reconcile.Rebuilt,
			Expired: e.Reconcile.Expired,
			Failed:  e.Reconcile.Failed,
		}
	}
	return s
}

func queueFromReadiness(check recall.ReadinessCheck) QueueSnapshot {
	return QueueSnapshot{
		Pending:        check.Pending,
		Leased:         check.Leased,
		ExpiredLeases:  check.ExpiredLeases,
		DeadLetter:     check.DeadLetter,
		Backlog:        check.Backlog,
		Completed:      check.Completed,
		CancelledTotal: check.CancelledTotal,
	}
}

func queueFromSideEffectProcess(res recall.SideEffectProcessResult) QueueSnapshot {
	return QueueSnapshot{
		Pending:    res.Claimed - res.Completed - res.Failed,
		Backlog:    res.Claimed,
		Completed:  res.Completed,
		DeadLetter: res.DeadLetter,
	}
}

func queueFromAsyncProcess(res recall.AsyncSemanticProcessResult) QueueSnapshot {
	return QueueSnapshot{
		Pending:   res.Claimed - res.Completed - res.Failed,
		Backlog:   res.Claimed,
		Completed: res.Completed,
	}
}
