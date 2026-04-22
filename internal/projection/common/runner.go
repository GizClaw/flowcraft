package projection

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// Config holds configuration for a Runner.
type Config struct {
	Name        string
	Log         eventlog.Log
	Snapshots   SnapshotStore  // optional; required when projector uses RestoreSnapshot
	DeadLetters DeadLetterSink // optional; defaults to LogDLT (slog.Warn)
	Projector   Projector

	// SnapshotsEnabled gates snapshot writes.
	SnapshotsEnabled bool

	// RestartBackoff controls retry sleep between transient apply failures.
	RestartBackoff time.Duration

	// ConsecutiveFailureThreshold: after this many failures on the same event
	// the runner writes the envelope to dead_letters and skips it.
	ConsecutiveFailureThreshold int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig(name string, log eventlog.Log, proj Projector) Config {
	return Config{
		Name:                        name,
		Log:                         log,
		Projector:                   proj,
		SnapshotsEnabled:            true,
		RestartBackoff:              250 * time.Millisecond,
		ConsecutiveFailureThreshold: 5,
	}
}

// Runner drives a single projector through restore → catchup → live.
//
// The lifecycle is:
//  1. restore() decides startSeq based on RestoreMode.
//  2. Subscribe(Since=startSeq) attaches with replay+live cut-over.
//  3. For each envelope, processEvent runs Apply + checkpoint inside a
//     single Log.Atomic. Failures retry up to ConsecutiveFailureThreshold,
//     after which the envelope is written to dead_letters and skipped.
//  4. Once Lag()==0 we mark the runner ready and call OnReady once.
type Runner struct {
	cfg    Config
	cancel context.CancelFunc
	ready  atomic.Bool
	status atomic.Pointer[Status]
	mu     sync.Mutex
}

// Status describes the current runner state for /readyz and /metrics.
type Status struct {
	CheckpointSeq       int64
	LatestSeq           int64
	Lag                 int64
	Ready               bool
	ConsecutiveFailures int
	LastError           string
}

// NewRunner creates a new projector runner.
func NewRunner(cfg Config) *Runner {
	r := &Runner{cfg: cfg}
	r.status.Store(&Status{})
	return r
}

func (r *Runner) Name() string   { return r.cfg.Name }
func (r *Runner) Status() Status { return *r.status.Load() }
func (r *Runner) IsReady() bool  { return r.ready.Load() }

// Start launches the projector goroutine; returns immediately.
func (r *Runner) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	go r.run(ctx)
}

// Stop signals the projector goroutine to exit.
func (r *Runner) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *Runner) run(ctx context.Context) {
	defer func() {
		if r.cancel != nil {
			r.cancel()
		}
	}()

	startSeq, err := r.restore(ctx)
	if err != nil {
		slog.Error("projector restore failed", "name", r.cfg.Name, "err", err)
		return
	}
	slog.Info("projector restored", "name", r.cfg.Name, "startSeq", startSeq)
	r.updateStatus(func(s *Status) { s.CheckpointSeq = startSeq })

	since := eventlog.Since(startSeq)
	if startSeq == 0 && r.cfg.Projector.RestoreMode() == RestoreReplay {
		since = eventlog.SinceBeginning
	}

	var partitions []string
	if pf, ok := r.cfg.Projector.(PartitionFilter); ok {
		partitions = pf.Partitions()
	}

	sub, err := r.cfg.Log.Subscribe(ctx, eventlog.SubscribeOptions{
		Partitions: partitions,
		Types:      r.cfg.Projector.Subscribes(),
		Since:      since,
		BufferSize: 256,
		OnLag:      r.onLag,
	})
	if err != nil {
		slog.Error("projector subscribe failed", "name", r.cfg.Name, "err", err)
		return
	}
	defer sub.Close()

	// Take a snapshot of LatestSeq right after attach: the runner is ready
	// once it has processed every event up to (and including) this seq.
	// New events arriving after Subscribe are part of the live stream and
	// don't gate readiness.
	readyTarget, err := r.cfg.Log.LatestSeq(ctx)
	if err != nil {
		slog.Error("projector LatestSeq failed", "name", r.cfg.Name, "err", err)
		return
	}
	if startSeq >= readyTarget {
		r.becomeReady(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			r.processEvent(ctx, env)
			if !r.ready.Load() && env.Seq >= readyTarget {
				r.becomeReady(ctx)
			}
		}
	}
}

func (r *Runner) restore(ctx context.Context) (int64, error) {
	mode := r.cfg.Projector.RestoreMode()
	cp, err := r.cfg.Log.Checkpoints().Get(ctx, r.cfg.Name)
	if err != nil {
		return 0, err
	}
	switch mode {
	case RestoreReplay:
		return cp, nil

	case RestoreSnapshot:
		snap, ok := r.cfg.Projector.(Snapshotter)
		if !ok {
			return cp, nil
		}
		if r.cfg.Snapshots == nil {
			return cp, nil
		}
		entry, err := r.cfg.Snapshots.Latest(ctx, r.cfg.Name)
		if err != nil {
			return 0, err
		}
		if entry == nil {
			return cp, nil
		}
		if entry.FormatVersion != snap.SnapshotFormatVersion() {
			return 0, ErrSnapshotIncompatible
		}
		if err := snap.LoadSnapshot(ctx, entry.Cursor, entry.Payload); err != nil {
			return 0, err
		}
		// The checkpoint may be older than the snapshot if the runner crashed
		// after writing the snapshot but before persisting the checkpoint;
		// resume at whichever is further along.
		if entry.Cursor > cp {
			return entry.Cursor, nil
		}
		return cp, nil

	case RestoreWindow:
		win, ok := r.cfg.Projector.(Windowed)
		if !ok {
			return cp, nil
		}
		latestSeq, err := r.cfg.Log.LatestSeq(ctx)
		if err != nil {
			return 0, err
		}
		// Approximation: assume ~1 event/sec; a tighter bound would scan
		// the log by timestamp, but for R2 this is enough to size the warm-up.
		windowSeq := int64(win.WindowSize().Seconds())
		startSeq := latestSeq - windowSeq
		if startSeq < cp {
			startSeq = cp
		}
		if startSeq < 0 {
			startSeq = 0
		}
		return startSeq, nil
	}
	return cp, nil
}

func (r *Runner) processEvent(ctx context.Context, env eventlog.Envelope) {
	// Retry the apply+checkpoint atomic up to the failure threshold, then
	// dead-letter and advance the checkpoint past the bad event so the runner
	// makes progress.
	threshold := r.cfg.ConsecutiveFailureThreshold
	if threshold <= 0 {
		threshold = 1
	}
	var lastErr error
	for attempt := 1; attempt <= threshold; attempt++ {
		_, err := r.cfg.Log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
			if err := r.cfg.Projector.Apply(ctx, uow, env); err != nil {
				return err
			}
			return uow.CheckpointSet(ctx, r.cfg.Name, env.Seq)
		})
		if err == nil {
			r.updateStatus(func(s *Status) {
				s.CheckpointSeq = env.Seq
				s.ConsecutiveFailures = 0
				s.LastError = ""
			})
			return
		}
		lastErr = err
		r.updateStatus(func(s *Status) {
			s.ConsecutiveFailures = attempt
			s.LastError = err.Error()
		})
		if attempt < threshold && r.cfg.RestartBackoff > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.cfg.RestartBackoff):
			}
		}
	}

	slog.Error("projector apply failed past threshold; dead-lettering",
		"name", r.cfg.Name, "seq", env.Seq, "err", lastErr)
	r.writeDeadLetter(ctx, env, lastErr)
	// Advance checkpoint past the bad event in its own transaction so we
	// don't get stuck.
	if _, err := r.cfg.Log.Atomic(ctx, func(uow eventlog.UnitOfWork) error {
		return uow.CheckpointSet(ctx, r.cfg.Name, env.Seq)
	}); err != nil {
		slog.Error("projector advance checkpoint after DLT failed",
			"name", r.cfg.Name, "seq", env.Seq, "err", err)
	}
	r.updateStatus(func(s *Status) {
		s.CheckpointSeq = env.Seq
	})
}

func (r *Runner) writeDeadLetter(ctx context.Context, env eventlog.Envelope, applyErr error) {
	sink := r.cfg.DeadLetters
	if sink == nil {
		sink = LogDLT{}
	}
	_ = sink.Write(ctx, DeadLetter{
		ProjectorName: r.cfg.Name,
		Seq:           env.Seq,
		Type:          env.Type,
		Partition:     env.Partition,
		Payload:       env.Payload,
		Err:           applyErr.Error(),
		At:            time.Now().UTC(),
	})
}

func (r *Runner) becomeReady(ctx context.Context) {
	if !r.ready.CompareAndSwap(false, true) {
		return
	}
	r.updateStatus(func(s *Status) { s.Ready = true })
	if err := r.cfg.Projector.OnReady(ctx); err != nil {
		slog.Error("projector OnReady failed", "name", r.cfg.Name, "err", err)
	}
}

func (r *Runner) onLag(lag int64) {
	r.updateStatus(func(s *Status) { s.Lag = lag })
}

func (r *Runner) updateStatus(mut func(*Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.status.Load()
	mut(&cur)
	r.status.Store(&cur)
}

// ErrProjectorNotReady is returned from WaitReady if the timeout expires.
var ErrProjectorNotReady = errors.New("projector not ready within timeout")

// WaitReady blocks until the projector is ready or timeout elapses.
func (r *Runner) WaitReady(ctx context.Context, timeout time.Duration) error {
	if r.ready.Load() {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		if r.ready.Load() {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrProjectorNotReady
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
