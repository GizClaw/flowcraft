//go:build unix

package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestOutputLog_TrimAndSequenceGap(t *testing.T) {
	log := newOutputLog(100)
	log.append(ProcessStreamStdout, bytesOf(60))
	log.append(ProcessStreamStdout, bytesOf(60))
	log.append(ProcessStreamStdout, bytesOf(60))
	log.finish()

	if got := log.retainedSeqLocked(); got != 120 {
		t.Fatalf("retainedSeq = %d, want 120 (only the newest 60-byte chunk fits the 100-byte budget)", got)
	}

	// The first chunk was trimmed: reading from seq 0 must report the
	// gap instead of silently returning the tail.
	_, err := log.read(context.Background(), 0, 4096)
	if !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("read(0) error = %v, want ErrSequenceGap", err)
	}

	out, err := log.read(context.Background(), 120, 4096)
	if err != nil {
		t.Fatalf("read(120): %v", err)
	}
	if out.NextSeq != 180 || len(out.Chunks) != 1 || len(out.Chunks[0].Data) != 60 {
		t.Fatalf("read(120) = %+v, want 60 bytes ending at seq 180", out)
	}
	if !out.EOF {
		t.Fatal("reading the tail after finish must set EOF")
	}

	tail, err := log.read(context.Background(), 180, 4096)
	if err != nil {
		t.Fatalf("read(120): %v", err)
	}
	if !tail.EOF || len(tail.Chunks) != 0 {
		t.Fatalf("caught-up read = %+v, want empty chunks + EOF", tail)
	}
}

func TestOutputLog_SingleChunkOverBudgetStaysReplayable(t *testing.T) {
	log := newOutputLog(10)
	log.append(ProcessStreamTTY, bytesOf(100))
	log.finish()

	out, err := log.read(context.Background(), 0, 4096)
	if err != nil {
		t.Fatalf("read(0): %v", err)
	}
	if out.NextSeq != 100 || len(out.Chunks[0].Data) != 100 {
		t.Fatalf("single oversized chunk must stay readable, got %+v", out)
	}
}

func TestOutputLog_ReadBlocksThenCtxCancel(t *testing.T) {
	log := newOutputLog(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := log.read(ctx, 0, 16)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read on canceled ctx = %v, want context.Canceled", err)
	}

	// A blocked read wakes when output arrives.
	ctx = context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := log.read(ctx, 0, 16)
		done <- err
	}()
	log.append(ProcessStreamStdout, []byte("hello"))
	if err := <-done; err != nil {
		t.Fatalf("blocked read after append: %v", err)
	}
}

func TestOutputLog_CloseWakesReaders(t *testing.T) {
	log := newOutputLog(0)
	done := make(chan error, 1)
	go func() {
		_, err := log.read(context.Background(), 0, 16)
		done <- err
	}()
	log.close()
	if err := <-done; !errors.Is(err, ErrProcessClosed) {
		t.Fatalf("read after close = %v, want ErrProcessClosed", err)
	}
}

func TestOutputLog_FutureSeqIsValidation(t *testing.T) {
	log := newOutputLog(0)
	log.append(ProcessStreamStdout, bytesOf(5))
	_, err := log.read(context.Background(), 99, 16)
	if !errdefs.IsValidation(err) {
		t.Fatalf("future afterSeq error = %v, want Validation", err)
	}
}

func bytesOf(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}
	return out
}
