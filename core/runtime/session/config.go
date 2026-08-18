package session

import (
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Tunables are the manager-level session settings a generation reload
// may change. Semantics:
//
//   - IdleTimeout and MaxSessions are read live by the manager, so a
//     change applies immediately (future timers and open checks).
//   - SinkBuffer, SpeculativeEvents, SpeculativeBytes, and
//     DeliveryConcurrency are captured by each Session at creation, so
//     a change applies to sessions created afterwards.
type Tunables struct {
	IdleTimeout         time.Duration
	SinkBuffer          int
	SpeculativeEvents   int
	SpeculativeBytes    int
	DeliveryConcurrency int
	MaxSessions         int
}

// UpdateTunables validates and atomically applies new manager tunables.
// Validation mirrors the With* ManagerOptions (positive bounds).
func (m *Manager) UpdateTunables(t Tunables) error {
	if m == nil {
		return errdefs.Validationf("runtime session: manager is nil")
	}
	opts := managerOptions{}
	for _, option := range []ManagerOption{
		WithIdleTimeout(t.IdleTimeout),
		WithSinkBufferSize(t.SinkBuffer),
		WithSpeculativeBufferLimits(t.SpeculativeEvents, t.SpeculativeBytes),
		WithDeliveryConcurrency(t.DeliveryConcurrency),
		WithMaxSessions(t.MaxSessions),
	} {
		if err := option(&opts); err != nil {
			return err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	m.idleTimeout = opts.idleTimeout
	m.sinkBuffer = opts.sinkBuffer
	m.speculativeEvents = opts.speculativeEvents
	m.speculativeBytes = opts.speculativeBytes
	m.deliveryConcurrency = opts.deliveryConcurrency
	m.maxSessions = opts.maxSessions
	return nil
}
