package session

import (
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

const (
	defaultIdleTimeout             = 10 * time.Minute
	defaultSinkBuffer              = 256
	defaultSpeculativeBufferEvents = 1024
	defaultSpeculativeBufferBytes  = 1 << 20
	defaultDeliveryConcurrency     = 8
	defaultMaxSessions             = 1024
)

type managerOptions struct {
	idleTimeout         time.Duration
	sinkBuffer          int
	speculativeEvents   int
	speculativeBytes    int
	deliveryConcurrency int
	maxSessions         int
	checkpoints         agent.CheckpointStore
	resume              bool
	observer            SessionObserver
	catalogProvider     CatalogProvider
}

// WithDeliveryConcurrency bounds how many sink callbacks may be in flight
// concurrently for one sink.
func WithDeliveryConcurrency(limit int) ManagerOption {
	return func(options *managerOptions) error {
		if limit <= 0 {
			return errdefs.Validationf(
				"runtime session: delivery concurrency must be positive")
		}
		options.deliveryConcurrency = limit
		return nil
	}
}

// WithMaxSessions bounds the number of distinct live sessions the manager
// retains. Values <= 0 leave the manager default (1024).
func WithMaxSessions(limit int) ManagerOption {
	return func(options *managerOptions) error {
		if limit <= 0 {
			return errdefs.Validationf("runtime session: max sessions must be positive")
		}
		options.maxSessions = limit
		return nil
	}
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

// WithSessionObserver attaches a session-level lifecycle observer to every
// Session created by this Manager. See SessionObserver for the callback
// contract.
func WithSessionObserver(observer SessionObserver) ManagerOption {
	return func(options *managerOptions) error {
		if isNil(observer) {
			return errdefs.Validationf("runtime session: session observer is required")
		}
		options.observer = observer
		return nil
	}
}

// WithCatalogProvider wires a per-session tool catalog provider. When
// set, every Session created by this Manager builds one catalog on its
// first Start, attaches it to each turn's execution context, and closes
// it when the Session closes.
func WithCatalogProvider(provider CatalogProvider) ManagerOption {
	return func(options *managerOptions) error {
		if isNil(provider) {
			return errdefs.Validationf("runtime session: catalog provider is required")
		}
		options.catalogProvider = provider
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

// WithCheckpointStore wires the store used for end-to-end resume. It
// is required when [WithResume] is enabled.
func WithCheckpointStore(store agent.CheckpointStore) ManagerOption {
	return func(options *managerOptions) error {
		if isNil(store) {
			return errdefs.Validationf("runtime session: checkpoint store is required")
		}
		options.checkpoints = store
		return nil
	}
}

// WithResume enables session-level resume: each session key maps to a
// stable run id, checkpoints are loaded before Start, and committed
// checkpoints are deleted after completion.
func WithResume(enable bool) ManagerOption {
	return func(options *managerOptions) error {
		options.resume = enable
		return nil
	}
}
