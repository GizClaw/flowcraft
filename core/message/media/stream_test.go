package media

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestPipeOrderedReadDrainThenEOF(t *testing.T) {
	pipe := NewPipe[string](3)
	for _, value := range []string{"a", "b", "c"} {
		if !pipe.Send(value) {
			t.Fatalf("Send(%q) rejected", value)
		}
	}
	ctx := context.Background()
	for _, want := range []string{"a", "b", "c"} {
		got, err := pipe.Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got != want {
			t.Fatalf("Read = %q, want %q", got, want)
		}
	}
	pipe.Close()
	if _, err := pipe.Read(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after Close = %v, want io.EOF", err)
	}
	if _, err := pipe.Read(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("subsequent Read = %v, want io.EOF", err)
	}
}

func TestPipeBufferBoundsBackpressure(t *testing.T) {
	pipe := NewPipe[int](1)
	if !pipe.Send(1) {
		t.Fatal("first Send rejected")
	}
	if pipe.TrySend(2) {
		t.Fatal("TrySend accepted a value on a full buffer")
	}

	done := make(chan bool, 1)
	go func() {
		done <- pipe.Send(2)
	}()
	select {
	case <-done:
		t.Fatal("Send completed while the buffer was full")
	case <-time.After(20 * time.Millisecond):
	}

	value, err := pipe.Read(context.Background())
	if err != nil || value != 1 {
		t.Fatalf("Read = (%v, %v), want (1, nil)", value, err)
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("blocked Send was rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Send did not complete after drain")
	}
}

func TestPipeInterruptSkipsBufferedValues(t *testing.T) {
	pipe := NewPipe[string](2)
	pipe.Send("stale-1")
	pipe.Send("stale-2")
	pipe.Interrupt()
	if _, err := pipe.Read(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read after Interrupt = %v, want context.Canceled", err)
	}
	if _, err := pipe.Read(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("subsequent Read = %v, want context.Canceled", err)
	}
}

func TestPipeInterruptTakesPrecedenceOverClose(t *testing.T) {
	pipe := NewPipe[int](1)
	pipe.Send(1)
	pipe.Interrupt()
	pipe.Close() // deferred Close must not mask the interrupt
	if _, err := pipe.Read(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read = %v, want context.Canceled", err)
	}
}

func TestPipeInterruptedSendRejected(t *testing.T) {
	pipe := NewPipe[int](0)
	pipe.Interrupt()
	if pipe.Send(1) {
		t.Fatal("Send succeeded on an interrupted pipe")
	}
	if pipe.TrySend(1) {
		t.Fatal("TrySend succeeded on an interrupted pipe")
	}
}

func TestPipeCallerContextCancellationIsPerCall(t *testing.T) {
	pipe := NewPipe[string](1)
	pipe.Send("kept")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pipe.Read(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read with canceled ctx = %v, want context.Canceled", err)
	}
	// The stream itself is untouched: a fresh context still sees the value.
	value, err := pipe.Read(context.Background())
	if err != nil || value != "kept" {
		t.Fatalf("Read = (%q, %v), want (\"kept\", nil)", value, err)
	}
}

func TestPipeCloseIdempotent(t *testing.T) {
	pipe := NewPipe[int](0)
	pipe.Close()
	pipe.Close()
	if _, err := pipe.Read(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Read = %v, want io.EOF", err)
	}
}
