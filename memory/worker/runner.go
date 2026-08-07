package worker

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/sources"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

type RunnerConfig struct {
	Processor *Processor
	Catalog   *sources.ScopeCatalog
	Scopes    []sdkmemory.Scope
	Interval  time.Duration
}

// Runner owns only an in-process cancellable scan loop. It is not a
// cross-process writer coordinator.
type Runner struct {
	processor *Processor
	catalog   *sources.ScopeCatalog
	scopes    []sdkmemory.Scope
	interval  time.Duration

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
	closed  bool
	lastErr error
}

func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.Processor == nil {
		return nil, errors.New("memory worker runner: processor is required")
	}
	if nilInterface(config.Catalog) {
		return nil, errors.New("memory worker runner: scope catalog is required")
	}
	if config.Interval <= 0 {
		return nil, errors.New("memory worker runner: interval must be positive")
	}
	scopes := append([]sdkmemory.Scope(nil), config.Scopes...)
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			return nil, err
		}
	}
	return &Runner{processor: config.Processor, catalog: config.Catalog, scopes: scopes, interval: config.Interval}, nil
}

func (runner *Runner) RunOnce(ctx context.Context) error {
	if runner == nil || runner.processor == nil {
		return errors.New("memory worker runner: runner is required")
	}
	if ctx == nil {
		return errors.New("memory worker runner: context is required")
	}
	var failures []error
	for _, scope := range runner.scopes {
		if err := runner.catalog.Register(ctx, scope); err != nil {
			failures = append(failures, err)
		}
	}
	scopes, err := runner.catalog.List(ctx)
	if err != nil {
		failures = append(failures, err)
		return errors.Join(failures...)
	}
	sort.Slice(scopes, func(i, j int) bool {
		return scopes[i].HardPartitionKey() < scopes[j].HardPartitionKey()
	})
	for _, scope := range scopes {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if err := runner.processor.ProcessScope(ctx, scope); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// ProcessScope runs derivation for exactly one hard scope without scanning
// every catalog scope. It is safe to call before or instead of RunOnce.
func (runner *Runner) ProcessScope(ctx context.Context, scope sdkmemory.Scope) error {
	if runner == nil || runner.processor == nil {
		return errors.New("memory worker runner: runner is incomplete")
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	return runner.processor.ProcessScope(ctx, scope)
}

// Start begins a loop whose first scan runs immediately.
func (runner *Runner) Start(ctx context.Context) error {
	if runner == nil {
		return errors.New("memory worker runner: runner is required")
	}
	if ctx == nil {
		return errors.New("memory worker runner: context is required")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.closed {
		return errors.New("memory worker runner: runner is closed")
	}
	if runner.started {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel
	runner.done = make(chan struct{})
	runner.started = true
	go runner.loop(runCtx, runner.done)
	return nil
}

func (runner *Runner) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	runner.scan(ctx)
	ticker := time.NewTicker(runner.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runner.scan(ctx)
		}
	}
}

func (runner *Runner) scan(ctx context.Context) {
	err := runner.RunOnce(ctx)
	runner.mu.Lock()
	runner.lastErr = err
	runner.mu.Unlock()
}

// LastError returns the most recent complete scan error.
func (runner *Runner) LastError() error {
	if runner == nil {
		return nil
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.lastErr
}

// Close is idempotent and waits for the loop to exit.
func (runner *Runner) Close() error {
	if runner == nil {
		return nil
	}
	runner.mu.Lock()
	if runner.closed {
		done := runner.done
		runner.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	runner.closed = true
	cancel := runner.cancel
	done := runner.done
	runner.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}
