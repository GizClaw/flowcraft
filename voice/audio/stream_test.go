package audio_test

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/voice/audio"
)

func TestPipe_SendRead(t *testing.T) {
	pipe := audio.NewPipe[int](4)

	pipe.Send(42)
	v, err := pipe.Read()
	if err != nil {
		t.Fatalf("Read: unexpected error %v", err)
	}
	if v != 42 {
		t.Errorf("Read: got %d, want 42", v)
	}
}

func TestPipe_CloseEOF(t *testing.T) {
	pipe := audio.NewPipe[int](4)
	pipe.Close()

	_, err := pipe.Read()
	if err != io.EOF {
		t.Errorf("Read: got error %v, want io.EOF", err)
	}
}

func TestPipe_CloseAfterBuffer(t *testing.T) {
	pipe := audio.NewPipe[int](4)

	for i := 0; i < 3; i++ {
		if !pipe.Send(i) {
			t.Fatalf("Send(%d) returned false", i)
		}
	}
	pipe.Close()

	for i := 0; i < 3; i++ {
		v, err := pipe.Read()
		if err != nil {
			t.Fatalf("Read %d: unexpected error %v", i, err)
		}
		if v != i {
			t.Errorf("Read %d: got %d, want %d", i, v, i)
		}
	}

	_, err := pipe.Read()
	if err != io.EOF {
		t.Errorf("Read after buffer: got error %v, want io.EOF", err)
	}
}

func TestPipe_InterruptError(t *testing.T) {
	pipe := audio.NewPipe[int](4)
	pipe.Interrupt()

	_, err := pipe.Read()
	if err != context.Canceled {
		t.Errorf("Read: got error %v, want context.Canceled", err)
	}
}

func TestPipe_InterruptSkipsBuffer(t *testing.T) {
	pipe := audio.NewPipe[int](4)

	for i := 0; i < 3; i++ {
		if !pipe.Send(i) {
			t.Fatalf("Send(%d) returned false", i)
		}
	}
	pipe.Interrupt()

	// Interrupt is deterministic: the very first Read skips all buffered
	// values and returns context.Canceled.
	if _, err := pipe.Read(); err != context.Canceled {
		t.Errorf("Read: got error %v, want context.Canceled", err)
	}

	// Subsequent reads also return the same error
	if _, err := pipe.Read(); err != context.Canceled {
		t.Errorf("subsequent Read: got error %v, want context.Canceled", err)
	}
}

func TestPipe_InterruptBeforeRead(t *testing.T) {
	pipe := audio.NewPipe[int](4)
	pipe.Interrupt()

	_, err := pipe.Read()
	if err != context.Canceled {
		t.Errorf("Read: got error %v, want context.Canceled", err)
	}
}

func TestPipe_SendAfterInterrupt(t *testing.T) {
	pipe := audio.NewPipe[int](4)
	pipe.Interrupt()

	if pipe.Send(42) {
		t.Error("Send after Interrupt: expected false, got true")
	}
}

func TestPipe_ConcurrentSendRead(t *testing.T) {
	pipe := audio.NewPipe[int](16)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			pipe.Send(i)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _ = pipe.Read()
		}
	}()

	wg.Wait()
}

func TestPipe_DoubleClose(t *testing.T) {
	pipe := audio.NewPipe[int](4)

	pipe.Close()
	pipe.Close() // must not panic
}

func TestPipe_InterruptThenClose(t *testing.T) {
	pipe := audio.NewPipe[int](4)

	pipe.Interrupt()
	pipe.Close() // must not panic

	// Interrupt takes precedence over Close: Read must deterministically
	// report context.Canceled, never a clean io.EOF, so an abnormal
	// termination is never observed as a normal end-of-stream.
	if _, err := pipe.Read(); err != context.Canceled {
		t.Errorf("Read after Interrupt+Close: got %v, want context.Canceled", err)
	}
}
