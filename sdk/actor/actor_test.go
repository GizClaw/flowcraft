package actor

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestActor_SendAndReceive(t *testing.T) {
	a := New(func(ctx context.Context, req int) (int, error) {
		return req * 2, nil
	})
	defer a.Stop()

	result := <-a.Send(21)
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Value != 42 {
		t.Fatalf("got %d, want 42", result.Value)
	}
}

func TestActor_SerialExecution(t *testing.T) {
	var order []int
	a := New(func(ctx context.Context, req int) (int, error) {
		order = append(order, req)
		return req, nil
	})
	defer a.Stop()

	for i := 0; i < 5; i++ {
		<-a.Send(i)
	}

	if len(order) != 5 {
		t.Fatalf("expected 5 executions, got %d", len(order))
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("order[%d] = %d, want %d", i, v, i)
		}
	}
}

func TestActor_StopRejectsSend(t *testing.T) {
	a := New(func(ctx context.Context, req int) (int, error) {
		return req, nil
	})
	a.Stop()

	result := <-a.Send(1)
	if result.Err != ErrStopped {
		t.Fatalf("expected ErrStopped, got %v", result.Err)
	}
}

func TestActor_Options(t *testing.T) {
	a := New(func(ctx context.Context, req int) (int, error) {
		return req, nil
	}, WithPersistent(), WithInboxSize(32), WithSource("test"))
	defer a.Stop()

	if !a.IsPersistent() {
		t.Fatal("expected persistent")
	}
	if a.Source() != "test" {
		t.Fatalf("source = %q, want \"test\"", a.Source())
	}
}

func TestActor_LastActive(t *testing.T) {
	before := time.Now()
	a := New(func(ctx context.Context, req int) (int, error) {
		return req, nil
	})
	defer a.Stop()

	<-a.Send(1)
	time.Sleep(10 * time.Millisecond)

	if a.LastActive().Before(before) {
		t.Fatal("LastActive should be updated after Send")
	}
}

func TestActor_Abort(t *testing.T) {
	var callCount int
	started := make(chan struct{})
	a := New(func(ctx context.Context, req int) (int, error) {
		callCount++
		if callCount == 1 {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return req * 2, nil
	})
	defer a.Stop()

	done := a.Send(1)
	<-started

	aborted := a.Abort()
	if !aborted {
		t.Fatal("Abort should return true when a request is running")
	}

	result := <-done
	if result.Err == nil {
		t.Fatal("expected context.Canceled error")
	}

	// Actor should still accept new messages
	result2 := <-a.Send(21)
	if result2.Err != nil {
		t.Fatalf("expected no error after abort, got %v", result2.Err)
	}
	if result2.Value != 42 {
		t.Fatalf("got %d, want 42", result2.Value)
	}
}

func TestActor_AbortWhenIdle(t *testing.T) {
	a := New(func(ctx context.Context, req int) (int, error) {
		return req, nil
	})
	defer a.Stop()

	// Ensure actor is idle
	<-a.Send(1)
	time.Sleep(10 * time.Millisecond)

	if a.Abort() {
		t.Fatal("Abort on idle actor should return false")
	}
}

func TestActor_AbortIdempotent(t *testing.T) {
	started := make(chan struct{})
	a := New(func(ctx context.Context, req int) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	defer a.Stop()

	a.Send(1)
	<-started

	first := a.Abort()
	second := a.Abort()
	if !first {
		t.Fatal("first Abort should return true")
	}
	if second {
		t.Fatal("second Abort should return false (already cancelled)")
	}
}

func TestActor_WithContext(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	a := New(func(ctx context.Context, req int) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	}, WithContext(parentCtx))

	done := a.Send(1)
	<-started

	parentCancel()

	result := <-done
	if result.Err == nil {
		t.Fatal("expected context cancelled error from parent cancel")
	}
}

func TestActor_StopCascadesCancelToMsgCtx(t *testing.T) {
	started := make(chan struct{})
	ctxCancelled := make(chan struct{})

	a := New(func(ctx context.Context, req int) (int, error) {
		close(started)
		<-ctx.Done()
		close(ctxCancelled)
		return 0, ctx.Err()
	})

	done := a.Send(1)
	<-started

	a.Stop()

	select {
	case <-ctxCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop should cascade-cancel the per-message context")
	}

	result := <-done
	if result.Err == nil {
		t.Fatal("expected context cancelled error")
	}
}

func TestActor_AbortConcurrentWithSend(t *testing.T) {
	a := New(func(ctx context.Context, req int) (int, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(10 * time.Millisecond):
			return req, nil
		}
	}, WithInboxSize(64))
	defer a.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := <-a.Send(1)
			_ = result
		}()
		if i%5 == 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a.Abort()
			}()
		}
	}
	wg.Wait()
}

func TestActor_SendAfterDrain_ReturnsErrStopped(t *testing.T) {
	a := New(func(ctx context.Context, req int) (int, error) {
		return req, nil
	})
	a.Stop()
	<-a.Done()

	for i := 0; i < 100; i++ {
		result := <-a.Send(i)
		if result.Err != ErrStopped {
			t.Fatalf("iteration %d: expected ErrStopped, got %v", i, result.Err)
		}
	}
}

func TestActor_StopConcurrentWithSend_NeverBlocks(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		a := New(func(ctx context.Context, req int) (int, error) {
			time.Sleep(time.Millisecond)
			return req, nil
		}, WithInboxSize(2))

		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result := <-a.Send(1)
				if result.Err != nil && result.Err != ErrStopped {
					t.Errorf("unexpected error: %v", result.Err)
				}
			}()
		}

		time.Sleep(time.Millisecond)
		a.Stop()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("trial %d: goroutines blocked after Stop", trial)
		}
	}
}

func TestActor_DrainPendingMessages(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	a := New(func(ctx context.Context, req int) (int, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return 0, ctx.Err()
	}, WithInboxSize(16))

	// Send first message that blocks in handler until Stop
	first := a.Send(0)
	<-started

	// Queue pending messages while handler is blocked
	var pending []<-chan Result[int]
	for i := 1; i <= 5; i++ {
		pending = append(pending, a.Send(i))
	}

	a.Stop()

	// First message should get context cancelled
	select {
	case result := <-first:
		if result.Err == nil {
			t.Fatal("first: expected error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first: blocked")
	}

	// Pending messages must not block — they should receive an error
	// (either ErrStopped from drain, or context.Canceled if processed
	// after Stop cancelled the base context).
	for i, ch := range pending {
		select {
		case result := <-ch:
			if result.Err == nil {
				t.Fatalf("pending[%d]: expected error, got success", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("pending[%d]: blocked waiting for result", i)
		}
	}
}
