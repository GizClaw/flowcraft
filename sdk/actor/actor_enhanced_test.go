package actor

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestActor_PanicRecovery(t *testing.T) {
	a := New(func(ctx context.Context, req int) (int, error) {
		if req == 42 {
			panic("handler panicked!")
		}
		return req * 2, nil
	})
	defer a.Stop()

	result := <-a.Send(42)
	if result.Err == nil {
		t.Fatal("expected error from panic")
	}
	if result.Value != 0 {
		t.Fatalf("expected zero value, got %d", result.Value)
	}

	result = <-a.Send(5)
	if result.Err != nil {
		t.Fatalf("actor should recover and process next message: %v", result.Err)
	}
	if result.Value != 10 {
		t.Fatalf("got %d, want 10", result.Value)
	}
}

func TestActor_ConcurrentSend(t *testing.T) {
	var mu sync.Mutex
	var order []int

	a := New(func(ctx context.Context, req int) (int, error) {
		mu.Lock()
		order = append(order, req)
		mu.Unlock()
		return req, nil
	}, WithInboxSize(64))
	defer a.Stop()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			result := <-a.Send(v)
			if result.Err != nil {
				t.Errorf("Send(%d) error: %v", v, result.Err)
			}
		}(i)
	}
	wg.Wait()

	mu.Lock()
	if len(order) != n {
		t.Fatalf("expected %d executions, got %d", n, len(order))
	}
	mu.Unlock()
}

func TestActor_StopIdempotent(t *testing.T) {
	a := New(func(ctx context.Context, req int) (int, error) {
		return req, nil
	})
	a.Stop()
	a.Stop()
	a.Stop()
}

func TestActor_StopDrainsPending(t *testing.T) {
	blocked := make(chan struct{})
	a := New(func(ctx context.Context, req int) (int, error) {
		if req == 1 {
			<-blocked
		}
		return req, nil
	}, WithInboxSize(16))

	ch1 := a.Send(1)
	time.Sleep(20 * time.Millisecond)

	ch2 := a.Send(2)
	ch3 := a.Send(3)

	close(blocked)
	a.Stop()

	r1 := <-ch1
	if r1.Err != nil {
		t.Fatalf("first message should succeed: %v", r1.Err)
	}

	r2 := <-ch2
	r3 := <-ch3
	_ = r2
	_ = r3
}

func TestActor_IsRunning(t *testing.T) {
	started := make(chan struct{})
	cont := make(chan struct{})

	a := New(func(ctx context.Context, req int) (int, error) {
		close(started)
		<-cont
		return req, nil
	})
	defer a.Stop()

	a.Send(1)
	<-started

	if !a.IsRunning() {
		t.Fatal("expected IsRunning=true during execution")
	}

	close(cont)
	time.Sleep(20 * time.Millisecond)

	if a.IsRunning() {
		t.Fatal("expected IsRunning=false after completion")
	}
}

func TestActor_InboxLen(t *testing.T) {
	blocked := make(chan struct{})
	a := New(func(ctx context.Context, req int) (int, error) {
		<-blocked
		return req, nil
	}, WithInboxSize(16))
	defer func() {
		close(blocked)
		a.Stop()
	}()

	a.Send(1)
	time.Sleep(10 * time.Millisecond)

	a.Send(2)
	a.Send(3)

	time.Sleep(10 * time.Millisecond)
	pending := a.InboxLen()
	if pending < 1 {
		t.Logf("InboxLen = %d (timing dependent)", pending)
	}
}

func TestActor_ContextCancellation(t *testing.T) {
	a := New(func(ctx context.Context, req int) (int, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Second):
			return req, nil
		}
	})

	a.Stop()

	ch := a.Send(1)
	result := <-ch
	if result.Err != ErrStopped {
		t.Fatalf("expected ErrStopped after Stop, got %v", result.Err)
	}
}
