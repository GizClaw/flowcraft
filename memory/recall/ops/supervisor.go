package ops

import (
	"context"
	"sync"
)

// Supervisor starts one or more runner loops and stops them through context
// cancellation. It is intentionally small: callers still own signal handling,
// logging, and whether an error should restart the surrounding process.
type Supervisor struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	errs []error
}

// Start starts runner loops for each target. The returned Supervisor is stopped
// by calling Stop or by cancelling ctx.
func Start(ctx context.Context, runner *Runner, targets ...Target) (*Supervisor, error) {
	if runner == nil {
		return nil, validationf("nil runner")
	}
	if len(targets) == 0 {
		return nil, validationf("at least one target is required")
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Supervisor{cancel: cancel}
	for _, target := range targets {
		target := target
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := runner.Run(ctx, RunOptions{Target: target}); err != nil && ctx.Err() == nil {
				s.addErr(err)
				cancel()
			}
		}()
	}
	return s, nil
}

// Stop cancels every loop and waits for shutdown. It returns the first worker
// error observed before cancellation, if any.
func (s *Supervisor) Stop() error {
	if s == nil {
		return nil
	}
	s.cancel()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) == 0 {
		return nil
	}
	return s.errs[0]
}

func (s *Supervisor) addErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err)
}
