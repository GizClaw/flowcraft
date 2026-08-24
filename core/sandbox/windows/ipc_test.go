//go:build windows

package windows

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := SpawnRequest{
		Argv:           []string{"git", "status"},
		Cwd:            `C:\workspace`,
		Env:            []string{"PATH=C:\\bin"},
		TTY:            true,
		Rows:           24,
		Cols:           80,
		MaxOutputBytes: 4096,
	}
	if err := writeFrame(&buf, msgSpawn, want); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	kind, payload, err := readFrame(&buf)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if kind != msgSpawn {
		t.Fatalf("kind = %q, want %q", kind, msgSpawn)
	}
	var got SpawnRequest
	if err := decodePayload(kind, payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Argv) != 2 || got.Argv[0] != "git" || got.Cwd != want.Cwd || !got.TTY || got.Cols != 80 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestFrameRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, msgWrite, WriteRequest{Data: make([]byte, maxFrameSize)}); err == nil {
		t.Fatal("writeFrame accepted an oversized frame")
	}
}

func TestReadFrameRejectsBadLength(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff})
	if _, _, err := readFrame(&buf); err == nil {
		t.Fatal("readFrame accepted an oversized length")
	}
	buf.Reset()
	buf.Write([]byte{0, 0, 0, 0})
	if _, _, err := readFrame(&buf); err == nil {
		t.Fatal("readFrame accepted a zero length")
	}
}

func TestOutputBufferReadSemantics(t *testing.T) {
	b := newOutputBuffer(0) // unbounded
	b.append(sandbox.SessionStreamStdout, []byte("hello "))
	b.append(sandbox.SessionStreamStderr, []byte("world"))
	b.finish(sandbox.SessionExit{Code: 0, Reason: sandbox.SessionExited}, nil)

	out, err := b.read(context.Background(), 0, 64)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !out.EOF || out.NextSeq != 11 {
		t.Fatalf("out = %+v", out)
	}
	if len(out.Chunks) != 2 || out.Chunks[0].Stream != sandbox.SessionStreamStdout || string(out.Chunks[1].Data) != "world" {
		t.Fatalf("chunks = %+v", out.Chunks)
	}
}

func TestOutputBufferTruncatesToMaxBytes(t *testing.T) {
	b := newOutputBuffer(0)
	b.append(sandbox.SessionStreamStdout, []byte("abcdefghij"))
	b.finish(sandbox.SessionExit{Code: 0, Reason: sandbox.SessionExited}, nil)

	out, err := b.read(context.Background(), 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Chunks[0].Data) != "abcd" || out.EOF || out.NextSeq != 4 {
		t.Fatalf("out = %+v", out)
	}
	// Resume from the cursor.
	out2, err := b.read(context.Background(), 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(out2.Chunks[0].Data) != "efgh" {
		t.Fatalf("out2 = %+v", out2)
	}
}

func TestOutputBufferSequenceGap(t *testing.T) {
	b := newOutputBuffer(8)
	b.append(sandbox.SessionStreamStdout, []byte("0123456789")) // 10 bytes > 8 budget
	// The single chunk is retained whole (trim never drops the only
	// chunk), so gap behavior needs a second chunk to force a drop.
	b.append(sandbox.SessionStreamStdout, []byte("ABCDEFGHIJ"))
	b.finish(sandbox.SessionExit{Code: 0, Reason: sandbox.SessionExited}, nil)

	if _, err := b.read(context.Background(), 0, 64); !errors.Is(err, sandbox.ErrSequenceGap) {
		t.Fatalf("read(0) err = %v, want ErrSequenceGap", err)
	}
}

func TestOutputBufferEOFBlocksUntilFinish(t *testing.T) {
	b := newOutputBuffer(0)
	done := make(chan sandbox.SessionOutput, 1)
	go func() {
		out, err := b.read(context.Background(), 0, 64)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		done <- out
	}()
	select {
	case <-done:
		t.Fatal("read returned before any data")
	default:
	}
	b.append(sandbox.SessionStreamStdout, []byte("x"))
	b.finish(sandbox.SessionExit{Code: 0, Reason: sandbox.SessionExited}, nil)
	select {
	case out := <-done:
		if !out.EOF || string(out.Chunks[0].Data) != "x" {
			t.Fatalf("out = %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not wake after finish")
	}
}
