package ops

import (
	"context"
	"sync"
)

// Supervisor starts one or more runner loops and stops them through context
// cancellation. It is intentionally small: callers still own signal handling,
// logging, and whether an error should restart the surrounding process.
type Supervisor struct {
	runCtx context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup

	mu               sync.Mutex
	graceful         bool
	gracefulReleased bool
	errs             []error
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
	runCtx, cancel := context.WithCancel(ctx)
	s := &Supervisor{runCtx: runCtx, cancel: cancel, stop: make(chan struct{})}
	for _, target := range targets {
		target := target
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := runner.run(runCtx, s.stop, RunOptions{Target: target}); err != nil {
				if s.recordRunError(err) {
					cancel()
				}
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
	s.once.Do(func() {
		close(s.stop)
	})
	s.cancel()
	s.wg.Wait()
	return s.err()
}

// GracefulStop asks worker loops to stop before the next drain pass and waits
// for any in-flight drain to finish without cancelling its context.
func (s *Supervisor) GracefulStop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.beginGracefulStop()
	s.once.Do(func() {
		close(s.stop)
	})
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		if err := s.gracefulErr(); err != nil {
			s.cancel()
			return err
		}
		s.releaseGraceful()
		return nil
	case <-ctx.Done():
		s.cancel()
		return ctx.Err()
	}
}

func (s *Supervisor) beginGracefulStop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.graceful = true
}

func (s *Supervisor) releaseGraceful() {
	s.mu.Lock()
	s.gracefulReleased = true
	s.mu.Unlock()
	s.cancel()
}

func (s *Supervisor) gracefulErr() error {
	s.mu.Lock()
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.mu.Unlock()
		return err
	}
	released := s.gracefulReleased
	s.mu.Unlock()
	if released {
		return nil
	}
	return s.runCtx.Err()
}

func (s *Supervisor) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) == 0 {
		return nil
	}
	return s.errs[0]
}

func (s *Supervisor) recordRunError(err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	runErr := s.runCtx.Err()
	if runErr == nil || s.graceful {
		s.errs = append(s.errs, err)
	}
	return runErr == nil
}
