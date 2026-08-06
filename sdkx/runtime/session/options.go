package session

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	defaultIdleTimeout             = 10 * time.Minute
	defaultSinkBuffer              = 256
	defaultSpeculativeBufferEvents = 1024
	defaultSpeculativeBufferBytes  = 1 << 20
)

type managerOptions struct {
	idleTimeout       time.Duration
	sinkBuffer        int
	speculativeEvents int
	speculativeBytes  int
}

// WithSinkBufferSize sets the queue size used when SinkSpec.QueueSize is zero.
func WithSinkBufferSize(size int) ManagerOption {
	return func(options *managerOptions) error {
		if size <= 0 {
			return errdefs.Validationf("runtime session: sink buffer size must be positive")
		}
		options.sinkBuffer = size
		return nil
	}
}

// WithSpeculativeBufferLimits bounds aggregate pending confirmed branch data per turn.
func WithSpeculativeBufferLimits(events, bytes int) ManagerOption {
	return func(options *managerOptions) error {
		if events <= 0 || bytes <= 0 {
			return errdefs.Validationf("runtime session: speculative buffer limits must be positive")
		}
		options.speculativeEvents = events
		options.speculativeBytes = bytes
		return nil
	}
}

// ManagerOption configures a Manager.
type ManagerOption func(*managerOptions) error

// WithIdleTimeout sets how long an unleased, idle Session is retained.
func WithIdleTimeout(timeout time.Duration) ManagerOption {
	return func(options *managerOptions) error {
		if timeout <= 0 {
			return errdefs.Validationf("runtime session: idle timeout must be positive")
		}
		options.idleTimeout = timeout
		return nil
	}
}
