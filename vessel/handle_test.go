package vessel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
)

// TestHandle_OnTerminate_RunsBeforeWaitReturns is the central
// contract: every Wait caller MUST observe a state where the
// hook has already executed. We register a hook that flips a
// flag, then assert the flag is true the moment Wait returns.
func TestHandle_OnTerminate_RunsBeforeWaitReturns(t *testing.T) {
	t.Parallel()
	h := newHandle("run-1", "alpha")

	var hookRan atomic.Bool
	h.OnTerminate(func(_ *agent.Result, _ error) {
		hookRan.Store(true)
	})

	// Drive deliver from a separate goroutine so Wait actually
	// blocks. Otherwise we'd be testing the late-register path.
	go func() {
		time.Sleep(5 * time.Millisecond)
		h.deliver(&agent.Result{}, nil)
	}()

	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !hookRan.Load() {
		t.Fatalf("hook did not run before Wait returned")
	}
}

// TestHandle_OnTerminate_RunsBeforeDoneCloses asserts the same
// ordering for the <-h.Done() consumer path. After the channel
// fires, every registered hook must already be done.
func TestHandle_OnTerminate_RunsBeforeDoneCloses(t *testing.T) {
	t.Parallel()
	h := newHandle("run-2", "alpha")

	var hookRan atomic.Bool
	h.OnTerminate(func(_ *agent.Result, _ error) {
		hookRan.Store(true)
	})

	go h.deliver(&agent.Result{}, nil)

	<-h.Done()
	if !hookRan.Load() {
		t.Fatalf("hook did not run before Done closed")
	}
}

// TestHandle_OnTerminate_FiresInRegistrationOrder pins the
// ordering guarantee so consumers can layer (e.g. close OTel
// span before recording metrics that may reference it).
func TestHandle_OnTerminate_FiresInRegistrationOrder(t *testing.T) {
	t.Parallel()
	h := newHandle("run-3", "alpha")

	var (
		mu  sync.Mutex
		got []int
	)
	for i := 0; i < 5; i++ {
		i := i
		h.OnTerminate(func(_ *agent.Result, _ error) {
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		})
	}

	h.deliver(&agent.Result{}, nil)

	for idx, want := range []int{0, 1, 2, 3, 4} {
		if got[idx] != want {
			t.Fatalf("hook order = %v, want [0 1 2 3 4]", got)
		}
	}
}

// TestHandle_OnTerminate_ReceivesResultAndErr verifies hooks
// observe the exact same (result, err) pair Wait returns —
// otherwise hooks couldn't make decisions based on terminal
// state (e.g. only flush checkpoints on success).
func TestHandle_OnTerminate_ReceivesResultAndErr(t *testing.T) {
	t.Parallel()
	h := newHandle("run-4", "alpha")

	wantRes := &agent.Result{}
	wantErr := errors.New("boom")

	var (
		gotRes *agent.Result
		gotErr error
	)
	h.OnTerminate(func(res *agent.Result, err error) {
		gotRes = res
		gotErr = err
	})

	h.deliver(wantRes, wantErr)

	if gotRes != wantRes {
		t.Fatalf("hook got result %p, want %p", gotRes, wantRes)
	}
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("hook got err %v, want %v", gotErr, wantErr)
	}
}

// TestHandle_OnTerminate_PanicIsIsolated ensures one buggy hook
// can't block subsequent hooks or leave Wait hanging. Critical
// for the "any subsystem can register" contract.
func TestHandle_OnTerminate_PanicIsIsolated(t *testing.T) {
	t.Parallel()
	h := newHandle("run-5", "alpha")

	var laterRan atomic.Bool
	h.OnTerminate(func(_ *agent.Result, _ error) {
		panic("hook 1 explodes")
	})
	h.OnTerminate(func(_ *agent.Result, _ error) {
		laterRan.Store(true)
	})

	h.deliver(&agent.Result{}, nil)

	if !laterRan.Load() {
		t.Fatalf("later hook did not run after earlier hook panicked")
	}
	// Wait must still return cleanly.
	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestHandle_OnTerminate_LateRegisterFastPath verifies hooks
// registered AFTER the run has already terminated are invoked
// synchronously with the cached terminal state. Otherwise late
// registration would silently drop bookkeeping.
func TestHandle_OnTerminate_LateRegisterFastPath(t *testing.T) {
	t.Parallel()
	h := newHandle("run-6", "alpha")

	wantErr := errors.New("post-mortem")
	h.deliver(&agent.Result{}, wantErr)

	var gotErr error
	h.OnTerminate(func(_ *agent.Result, err error) {
		gotErr = err
	})

	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("late hook got err %v, want %v", gotErr, wantErr)
	}
}

// TestHandle_OnTerminate_NilFnIsNoop guards against nil-fn
// crashes — registering nil should be a silent no-op rather
// than a panic at deliver time.
func TestHandle_OnTerminate_NilFnIsNoop(t *testing.T) {
	t.Parallel()
	h := newHandle("run-7", "alpha")
	h.OnTerminate(nil)
	h.deliver(&agent.Result{}, nil)
	// Reaching here without panic is the assertion.
}

// TestHandle_OnTerminate_ConcurrentRegistration stresses the
// registration path against deliver to confirm no hook is lost
// AND no hook runs twice. Race detector is the primary check.
func TestHandle_OnTerminate_ConcurrentRegistration(t *testing.T) {
	t.Parallel()
	h := newHandle("run-8", "alpha")

	const N = 32
	var ran atomic.Int32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			h.OnTerminate(func(_ *agent.Result, _ error) {
				ran.Add(1)
			})
		}()
	}

	go func() {
		// Race deliver against the registration storm.
		time.Sleep(time.Microsecond)
		h.deliver(&agent.Result{}, nil)
	}()

	wg.Wait()
	// wg.Wait covers OnTerminate returns; deliver may still be
	// mid-loop iterating queued hooks. Done is closed only after
	// every queued hook has executed, so it's the right barrier.
	<-h.Done()
	if got := ran.Load(); got != N {
		t.Fatalf("ran = %d, want %d (each hook must fire exactly once)", got, N)
	}
}
