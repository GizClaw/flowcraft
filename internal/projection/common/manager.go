package projection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// ManagerConfig holds Manager-level config.
type ManagerConfig struct {
	ReadyTimeout time.Duration
}

// DefaultManagerConfig returns sensible defaults.
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{ReadyTimeout: 60 * time.Second}
}

// Manager owns the lifecycle of every projector + scheduler in the process.
//
// Registration is closed once Start is called: nothing else may be added.
// Manager.Start runs nodes in topological order, each waiting for its
// predecessors to become ready before it starts so that downstream consumers
// observe a consistent view.
type Manager struct {
	cfg       ManagerConfig
	mu        sync.RWMutex
	registry  map[string]*node
	order     []string
	started   bool
	stopOnce  sync.Once
	startedAt time.Time
}

type nodeKind int

const (
	nodeProjector nodeKind = iota
	nodeScheduler
)

type node struct {
	name      string
	kind      nodeKind
	dependsOn []string

	// projector-only fields
	runner *Runner
	rcOpts runnerOptions

	// scheduler-only fields
	run    func(ctx context.Context) error
	cancel context.CancelFunc
	ready  chan struct{}
}

type runnerOptions struct {
	snapshots   bool
	snapshotter SnapshotStore
	dlt         DeadLetterSink
}

func defaultRunnerOptions() runnerOptions {
	return runnerOptions{snapshots: true}
}

// RegisterOption tunes a single projector registration.
type RegisterOption func(*runnerOptions)

// WithSnapshotsDisabled turns off snapshot writes for this projector.
func WithSnapshotsDisabled() RegisterOption {
	return func(o *runnerOptions) { o.snapshots = false }
}

// WithSnapshotStore wires a SnapshotStore (required for RestoreSnapshot mode).
func WithSnapshotStore(s SnapshotStore) RegisterOption {
	return func(o *runnerOptions) { o.snapshotter = s }
}

// WithDeadLetterSink overrides the default DLT sink (LogDLT).
func WithDeadLetterSink(sink DeadLetterSink) RegisterOption {
	return func(o *runnerOptions) { o.dlt = sink }
}

// NewManager constructs an empty Manager.
func NewManager(cfg ManagerConfig) *Manager {
	return &Manager{cfg: cfg, registry: map[string]*node{}}
}

// RegisterProjector adds a projector to the dependency graph. Optionally
// declare other projector/scheduler names this projector waits on.
func (m *Manager) RegisterProjector(p Projector, dependsOn []string, opts ...RegisterOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return errors.New("projection: cannot register after Start")
	}
	if _, dup := m.registry[p.Name()]; dup {
		return fmt.Errorf("projection: duplicate name %q", p.Name())
	}
	o := defaultRunnerOptions()
	for _, opt := range opts {
		opt(&o)
	}
	m.registry[p.Name()] = &node{
		name:      p.Name(),
		kind:      nodeProjector,
		dependsOn: append([]string(nil), dependsOn...),
		rcOpts:    o,
		runner:    NewRunner(Config{Name: p.Name(), Projector: p}),
	}
	return nil
}

// RegisterScheduler adds a background scheduler job (e.g. cron tick, webhook
// dispatcher). dependsOn entries usually name projectors that must be ready
// before the scheduler starts firing.
func (m *Manager) RegisterScheduler(name string, run func(ctx context.Context) error, dependsOn ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return errors.New("projection: cannot register after Start")
	}
	if _, dup := m.registry[name]; dup {
		return fmt.Errorf("projection: duplicate name %q", name)
	}
	m.registry[name] = &node{
		name:      name,
		kind:      nodeScheduler,
		dependsOn: append([]string(nil), dependsOn...),
		run:       run,
		ready:     make(chan struct{}),
	}
	return nil
}

// Start fans out projectors and schedulers in topological order. Every node
// receives the same Log; projectors additionally get the SnapshotStore and
// DeadLetterSink configured at registration time. Start returns once every
// projector reports ready or the per-manager timeout elapses.
func (m *Manager) Start(ctx context.Context, log eventlog.Log) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("projection: manager already started")
	}
	m.started = true
	m.startedAt = time.Now()
	order, err := m.computeOrder()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.order = order
	for _, name := range order {
		n := m.registry[name]
		if n.kind == nodeProjector {
			n.runner.cfg.Log = log
			n.runner.cfg.SnapshotsEnabled = n.rcOpts.snapshots
			n.runner.cfg.Snapshots = n.rcOpts.snapshotter
			n.runner.cfg.DeadLetters = n.rcOpts.dlt
			if n.runner.cfg.RestartBackoff == 0 {
				n.runner.cfg.RestartBackoff = 250 * time.Millisecond
			}
			if n.runner.cfg.ConsecutiveFailureThreshold == 0 {
				n.runner.cfg.ConsecutiveFailureThreshold = 5
			}
		}
	}
	m.mu.Unlock()

	for _, name := range order {
		if err := m.startNode(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) startNode(ctx context.Context, name string) error {
	m.mu.RLock()
	n := m.registry[name]
	m.mu.RUnlock()

	switch n.kind {
	case nodeProjector:
		n.runner.Start(ctx)
		if err := n.runner.WaitReady(ctx, m.cfg.ReadyTimeout); err != nil {
			slog.Error("projection: projector not ready",
				"name", name, "err", err, "timeout", m.cfg.ReadyTimeout)
			return fmt.Errorf("projection: %s not ready: %w", name, err)
		}
		slog.Info("projection: projector ready", "name", name)
	case nodeScheduler:
		nodeCtx, cancel := context.WithCancel(ctx)
		n.cancel = cancel
		go func() {
			defer close(n.ready)
			if err := n.run(nodeCtx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("projection: scheduler exited with error",
					"name", name, "err", err)
			}
		}()
		// Schedulers are considered "ready" the moment the goroutine starts;
		// any further synchronization is the scheduler's responsibility.
	}
	return nil
}

// Stop cancels every scheduler and signals every projector to exit. Safe to
// call multiple times.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		m.mu.RLock()
		defer m.mu.RUnlock()
		for _, n := range m.registry {
			if n.kind == nodeScheduler && n.cancel != nil {
				n.cancel()
			}
			if n.kind == nodeProjector {
				n.runner.Stop()
			}
		}
	})
}

// IsAllReady reports whether every projector is ready. Schedulers don't
// participate in the readiness gate (they have no replay phase).
func (m *Manager) IsAllReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, n := range m.registry {
		if n.kind == nodeProjector && !n.runner.IsReady() {
			return false
		}
	}
	return true
}

// Status returns a snapshot of every projector's status.
func (m *Manager) Status() []ProjectorStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ProjectorStatus, 0, len(m.registry))
	for _, n := range m.registry {
		if n.kind != nodeProjector {
			continue
		}
		st := n.runner.Status()
		out = append(out, ProjectorStatus{
			Name:                n.name,
			CheckpointSeq:       st.CheckpointSeq,
			LatestSeq:           st.LatestSeq,
			Lag:                 st.Lag,
			Ready:               st.Ready,
			ConsecutiveFailures: st.ConsecutiveFailures,
			LastError:           st.LastError,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ProjectorStatus describes the current status of a projector for /readyz.
type ProjectorStatus struct {
	Name                string
	CheckpointSeq       int64
	LatestSeq           int64
	Lag                 int64
	Ready               bool
	ConsecutiveFailures int
	LastError           string
}

// computeOrder runs Kahn's algorithm over the registry. It returns an error
// if any dependency points at a name that wasn't registered or a cycle exists.
func (m *Manager) computeOrder() ([]string, error) {
	indeg := map[string]int{}
	deps := map[string][]string{}
	all := []string{}
	for name, n := range m.registry {
		all = append(all, name)
		indeg[name] = len(n.dependsOn)
		for _, d := range n.dependsOn {
			if _, ok := m.registry[d]; !ok {
				return nil, fmt.Errorf("projection: %q depends on unknown %q", name, d)
			}
			deps[d] = append(deps[d], name)
		}
	}
	sort.Strings(all)
	queue := []string{}
	for _, name := range all {
		if indeg[name] == 0 {
			queue = append(queue, name)
		}
	}
	order := make([]string, 0, len(all))
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		order = append(order, n)
		nexts := append([]string(nil), deps[n]...)
		sort.Strings(nexts)
		for _, next := range nexts {
			indeg[next]--
			if indeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(order) != len(all) {
		return nil, errors.New("projection: dependency cycle")
	}
	return order, nil
}
