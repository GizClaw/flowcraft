package sessionkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// The output log is the whole session contract in one type: an append-only,
// bounded, cursor-addressed buffer. These tests drive it directly, without a
// process, because that is where its edge cases live.

func TestOutputLogReadCursor(t *testing.T) {
	l := newOutputLog(0)
	defer l.close()
	l.append(sandbox.SessionStreamStdout, []byte("abc"))
	l.append(sandbox.SessionStreamStdout, []byte("def"))

	// A read stops at maxBytes and advances the cursor by exactly what it
	// returned, so two halves reassemble the stream.
	out, err := l.read(context.Background(), 0, 4)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.NextSeq != 4 || out.EOF {
		t.Fatalf("read = %+v, want NextSeq 4 without EOF", out)
	}
	if got := concatData(out.Chunks); got != "abcd" {
		t.Fatalf("first half = %q", got)
	}

	out, err = l.read(context.Background(), out.NextSeq, 4)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := concatData(out.Chunks); got != "ef" {
		t.Fatalf("second half = %q", got)
	}
	if out.NextSeq != 6 {
		t.Fatalf("NextSeq = %d, want 6", out.NextSeq)
	}

	// EOF is a value that arrives only once the process finished and the
	// cursor reached the end.
	l.finish(sandbox.SessionExit{Code: 0})
	out, err = l.read(context.Background(), 6, 16)
	if err != nil {
		t.Fatalf("read at end: %v", err)
	}
	if !out.EOF || len(out.Chunks) != 0 {
		t.Fatalf("read at end = %+v, want EOF with no chunks", out)
	}
	// The log stays readable after EOF.
	out, err = l.read(context.Background(), 0, 16)
	if err != nil {
		t.Fatalf("replay after EOF: %v", err)
	}
	if got := concatData(out.Chunks); got != "abcdef" {
		t.Fatalf("replay = %q", got)
	}
}

func TestOutputLogSequenceGap(t *testing.T) {
	// A 4-byte budget with two 4-byte writes: the first chunk is dropped, so a
	// cursor at 0 can no longer be served.
	l := newOutputLog(4)
	defer l.close()
	l.append(sandbox.SessionStreamStdout, []byte("aaaa"))
	l.append(sandbox.SessionStreamStdout, []byte("bbbb"))

	if _, err := l.read(context.Background(), 0, 16); !errors.Is(err, sandbox.ErrSequenceGap) {
		t.Fatalf("read from a dropped cursor = %v, want ErrSequenceGap", err)
	}
	// The retained range still serves.
	if out, err := l.read(context.Background(), 4, 16); err != nil || concatData(out.Chunks) != "bbbb" {
		t.Fatalf("read from the retained range = %q, %v", concatData(out.Chunks), err)
	}
}

func TestOutputLogReadRejectsBadCursors(t *testing.T) {
	l := newOutputLog(0)
	defer l.close()
	l.append(sandbox.SessionStreamStdout, []byte("abc"))

	if _, err := l.read(context.Background(), 0, 0); !errdefs.IsValidation(err) {
		t.Errorf("maxBytes=0: err = %v, want Validation", err)
	}
	if _, err := l.read(context.Background(), 99, 16); !errdefs.IsValidation(err) {
		t.Errorf("cursor past the end: err = %v, want Validation", err)
	}
}

func TestOutputLogReadAfterClose(t *testing.T) {
	l := newOutputLog(0)
	l.append(sandbox.SessionStreamStdout, []byte("abc"))
	l.close()

	if _, err := l.read(context.Background(), 0, 16); !errors.Is(err, sandbox.ErrSessionClosed) {
		t.Fatalf("read after close = %v, want ErrSessionClosed", err)
	}
}

// Read blocks until there is something to return, which is what makes a live
// session's output arrive without polling.
func TestOutputLogReadBlocksThenWakes(t *testing.T) {
	l := newOutputLog(0)
	defer l.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type result struct {
		out sandbox.SessionOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := l.read(ctx, 0, 16)
		done <- result{out, err}
	}()

	l.append(sandbox.SessionStreamStdout, []byte("late"))

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("read: %v", got.err)
		}
		if c := concatData(got.out.Chunks); c != "late" {
			t.Fatalf("read = %q, want the appended bytes", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read did not wake on append")
	}
}

// subscribe in the log only registers a queue: the replay that makes Watch
// "replay-then-live" happens in the caller, under the same lock. This pins that
// the log's own contract is "live from now on, no gap between the two".
func TestOutputLogSubscribeIsLiveOnly(t *testing.T) {
	l := newOutputLog(0)
	defer l.close()
	l.append(sandbox.SessionStreamStdout, []byte("before"))

	l.mu.Lock()
	w := l.subscribe()
	l.mu.Unlock()

	l.append(sandbox.SessionStreamStdout, []byte("after"))

	select {
	case ev := <-w.Events():
		if string(ev.Data) != "after" {
			t.Fatalf("first live event = %q, want only events after the subscription", ev.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live event")
	}
}

// A subscriber that cannot keep up is told where to resume instead of losing
// bytes silently: one Lag event naming the first cursor it did not receive, then
// the channel closes. Recovery is a Read from that cursor.
func TestOutputLogWatchLag(t *testing.T) {
	l := newOutputLog(0)
	defer l.close()

	l.mu.Lock()
	w := l.subscribe()
	l.mu.Unlock()

	const appends = watcherQueueSize + 8
	for range appends {
		l.append(sandbox.SessionStreamStdout, []byte("x"))
	}

	received := make(map[int64]bool, appends)
	var lagSeq int64 = -1
	closed := false
	deadline := time.After(5 * time.Second)
	for !closed {
		select {
		case ev, open := <-w.Events():
			if !open {
				closed = true
				continue
			}
			switch ev.Type {
			case sandbox.SessionEventOutput:
				received[ev.Seq] = true
			case sandbox.SessionEventLag:
				lagSeq = ev.Seq
			}
		case <-deadline:
			t.Fatalf("watcher did not report a lag (received=%d lagSeq=%d)", len(received), lagSeq)
		}
	}

	if lagSeq < 0 {
		t.Fatal("no lag event was delivered for an overflowing subscriber")
	}
	// The named cursor must be a cursor this consumer actually missed: the
	// smallest one it never received.
	firstMissing := int64(-1)
	for seq := int64(0); seq < appends; seq++ {
		if !received[seq] {
			firstMissing = seq
			break
		}
	}
	if firstMissing < 0 {
		t.Fatal("the consumer missed nothing, so a lag event was wrong")
	}
	if lagSeq != firstMissing {
		t.Fatalf("lag seq = %d, want the first missed cursor %d (received %d events)",
			lagSeq, firstMissing, len(received))
	}

	// Recovery is a Read from the named cursor: the bytes are still there.
	out, err := l.read(context.Background(), lagSeq, 1<<20)
	if err != nil {
		t.Fatalf("recovery read: %v", err)
	}
	if len(out.Chunks) == 0 {
		t.Fatal("recovery read returned nothing; the lag cursor does not resume")
	}
}

func TestOutputLogCloseDeliversClosed(t *testing.T) {
	l := newOutputLog(0)
	l.mu.Lock()
	w := l.subscribe()
	l.mu.Unlock()

	l.close()

	select {
	case ev := <-w.Events():
		if ev.Type != sandbox.SessionEventClosed {
			t.Fatalf("first event = %v, want closed", ev.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no closed event")
	}
	if _, open := <-w.Events(); open {
		t.Fatal("the channel stayed open after the closed event")
	}
}

// finish (the process exited) is not close (the handle went away): the exit is
// delivered, and the subscription stays open because the log is still readable.
func TestOutputLogFinishDeliversExited(t *testing.T) {
	l := newOutputLog(0)
	defer l.close()
	l.mu.Lock()
	w := l.subscribe()
	l.mu.Unlock()

	l.finish(sandbox.SessionExit{Code: 7})

	select {
	case ev := <-w.Events():
		if ev.Type != sandbox.SessionEventExited || ev.Exit == nil || ev.Exit.Code != 7 {
			t.Fatalf("event = %+v, want the exit", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit event")
	}

	select {
	case ev, open := <-w.Events():
		if !open {
			t.Fatal("the channel closed on exit; only Close does that")
		}
		t.Fatalf("unexpected event after exit: %+v", ev)
	case <-time.After(50 * time.Millisecond):
		// Still open and quiet: correct.
	}
}

func concatData(chunks []sandbox.OutputChunk) string {
	var out []byte
	for _, c := range chunks {
		out = append(out, c.Data...)
	}
	return string(out)
}
