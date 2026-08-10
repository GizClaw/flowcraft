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
	log.finish(ProcessExit{})

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
	log.finish(ProcessExit{})

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

func TestWatcher_ReplayThenLiveOrdering(t *testing.T) {
	log := newOutputLog(0)
	log.append(ProcessStreamStdout, []byte("one"))
	log.mu.Lock()
	w := log.subscribe()
	log.mu.Unlock()
	go w.run(context.Background())

	done := make(chan []ProcessEvent, 1)
	go func() {
		var got []ProcessEvent
		for ev := range w.Events() {
			got = append(got, ev)
		}
		done <- got
	}()

	log.append(ProcessStreamStderr, []byte("two"))
	log.finish(ProcessExit{Code: 7, Reason: ProcessExited})
	_ = w.Close()

	got := <-done
	// subscribe() is live-only (replay belongs to session.Watch), so
	// pre-subscribe output is not delivered.
	if len(got) != 2 {
		types := make([]string, len(got))
		for i, ev := range got {
			types[i] = ev.Type.String()
		}
		t.Fatalf("events = %d (%v), want live(1) + exited(1)", len(got), types)
	}
	if got[0].Type != ProcessEventOutput || string(got[0].Data) != "two" || got[0].Stream != ProcessStreamStderr {
		t.Fatalf("event[1] = %+v", got[1])
	}
	if got[1].Type != ProcessEventExited || got[1].Exit == nil || got[1].Exit.Code != 7 {
		t.Fatalf("event[1] = %+v, want Exited(7)", got[1])
	}
	if got[1].Seq != 6 {
		t.Fatalf("Exited seq = %d, want 6 (3+3 bytes)", got[1].Seq)
	}
}

func TestWatcher_LagThenClose(t *testing.T) {
	log := newOutputLog(0)
	log.mu.Lock()
	w := log.subscribe()
	log.mu.Unlock()
	go w.run(context.Background())

	for i := 0; i < 300; i++ {
		log.append(ProcessStreamStdout, []byte("x"))
	}

	done := make(chan []ProcessEvent, 1)
	go func() {
		var got []ProcessEvent
		for ev := range w.Events() {
			got = append(got, ev)
		}
		done <- got
	}()
	got := <-done
	if len(got) != watcherQueueSize {
		t.Fatalf("drained %d events, want queue capacity %d (one slot freed for Lag)", len(got), watcherQueueSize)
	}
	last := got[len(got)-1]
	if last.Type != ProcessEventLag {
		t.Fatalf("last event = %v, want ProcessEventLag", last.Type)
	}
	if last.Seq == 0 {
		t.Fatal("Lag must carry the first missed byte cursor")
	}
}

func TestWatcher_ClosedEventOnLogClose(t *testing.T) {
	log := newOutputLog(0)
	log.mu.Lock()
	w := log.subscribe()
	log.mu.Unlock()
	go w.run(context.Background())

	done := make(chan []ProcessEvent, 1)
	go func() {
		var got []ProcessEvent
		for ev := range w.Events() {
			got = append(got, ev)
		}
		done <- got
	}()
	log.close()
	got := <-done
	if len(got) != 1 || got[0].Type != ProcessEventClosed {
		t.Fatalf("events = %+v, want one Closed", got)
	}
}

func bytesOf(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}
	return out
}
