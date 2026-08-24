//go:build windows

package windows

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// This file holds the framed-pipe protocol and the client-side output
// buffer shared by the elevated sandbox runner (P2a). The protocol is
// deliberately small: one duplex named pipe, length-prefixed JSON
// messages, and the seq-cursor contract of core/sandbox carried
// verbatim over the wire.

// maxFrameSize bounds one encoded IPC message (output chunks are
// 32 KiB, so 1 MiB is generous headroom for metadata).
const maxFrameSize = 1 << 20

// Message kinds. Client -> server: control requests; server -> client:
// spawn outcome and streamed output.
const (
	msgSpawn      = "spawn"
	msgWrite      = "write"
	msgResize     = "resize"
	msgCloseInput = "close_input"
	msgTerminate  = "terminate"
	msgClose      = "close"
	msgShutdown   = "shutdown"

	msgReady  = "ready"
	msgOutput = "output"
	msgExit   = "exit"
	msgError  = "error"
)

// SpawnRequest is the elevated runner's spawn command. It mirrors the
// policy surface the unelevated backend already validated; the server
// re-validates mechanics only (via core/sandbox.StartWindowsSession).
type SpawnRequest struct {
	Argv           []string `json:"argv"`
	Cwd            string   `json:"cwd"`
	Env            []string `json:"env"`
	Root           string   `json:"root"`
	WritableRoots  []string `json:"writable_roots,omitempty"`
	Account        string   `json:"account"`
	Secret         string   `json:"secret"`
	TTY            bool     `json:"tty"`
	Rows           int      `json:"rows"`
	Cols           int      `json:"cols"`
	MaxOutputBytes int64    `json:"max_output_bytes"`
	MemoryBytes    int64    `json:"memory_bytes,omitempty"`
	CPUMillicores  int      `json:"cpu_millicores,omitempty"`
	TimeoutMs      int64    `json:"timeout_ms,omitempty"`
}

// SpawnReady reports the launched process and the session surface the
// elevated backend can actually provide.
type SpawnReady struct {
	PID  uint32                      `json:"pid"`
	Caps sandbox.SessionCapabilities `json:"caps"`
}

// OutputFrame carries one contiguous run of bytes with its sequence
// cursor, preserving the core/sandbox replay contract over the wire.
type OutputFrame struct {
	Seq    int64                 `json:"seq"`
	Stream sandbox.SessionStream `json:"stream"`
	Data   []byte                `json:"data"`
}

// ExitFrame is the terminal outcome of the session.
type ExitFrame struct {
	Exit sandbox.SessionExit `json:"exit"`
	Err  string              `json:"err,omitempty"`
}

// ErrorFrame reports a server-side failure at a named stage (spawn,
// write, resize, ...).
type ErrorFrame struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

type WriteRequest struct {
	Data []byte `json:"data"`
}

type ResizeRequest struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// ShutdownRequest asks the elevated helper to exit. It carries the
// same per-runner secret as SpawnRequest.
type ShutdownRequest struct {
	Secret string `json:"secret"`
}

// ipcEnvelope is the length-prefixed JSON frame on the wire.
type ipcEnvelope struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// writeFrame encodes kind+payload as one length-prefixed JSON frame.
func writeFrame(w io.Writer, kind string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("windows/elevated: encode %s: %w", kind, err)
	}
	buf, err := json.Marshal(ipcEnvelope{Kind: kind, Payload: body})
	if err != nil {
		return fmt.Errorf("windows/elevated: encode envelope: %w", err)
	}
	if len(buf) > maxFrameSize {
		return fmt.Errorf("windows/elevated: frame too large: %d", len(buf))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(buf)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, buf)
}

// readFrame decodes one length-prefixed JSON frame, returning its kind
// and raw payload. It guards against oversized or truncated frames.
func readFrame(r io.Reader) (kind string, payload json.RawMessage, err error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return "", nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > maxFrameSize {
		return "", nil, fmt.Errorf("windows/elevated: invalid frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", nil, err
	}
	var env ipcEnvelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return "", nil, fmt.Errorf("windows/elevated: decode envelope: %w", err)
	}
	return env.Kind, env.Payload, nil
}

// decodePayload unmarshals a frame payload into out.
func decodePayload(kind string, payload json.RawMessage, out any) error {
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("windows/elevated: decode %s: %w", kind, err)
	}
	return nil
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return fmt.Errorf("windows/elevated: write: %w", err)
		}
		b = b[n:]
	}
	return nil
}

// outputChunk is one retained run of output on the client side.
type outputChunk struct {
	seq    int64
	stream sandbox.SessionStream
	data   []byte
}

// outputBuffer is the client-side replay buffer for a pipe session. It
// implements the same bounded seq-cursor semantics as the server's
// core/sandbox output log, so Read contracts (ErrSequenceGap, EOF,
// truncation to maxBytes) hold identically on both ends of the pipe.
type outputBuffer struct {
	mu      sync.Mutex
	chunks  []outputChunk
	total   int64
	nextSeq int64
	max     int64
	eof     bool
	exit    sandbox.SessionExit
	waitErr error
	wake    chan struct{}
}

func newOutputBuffer(maxBytes int64) *outputBuffer {
	return &outputBuffer{max: maxBytes, wake: make(chan struct{})}
}

func (b *outputBuffer) append(stream sandbox.SessionStream, data []byte) {
	if len(data) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.eof {
		return
	}
	b.chunks = append(b.chunks, outputChunk{
		seq:    b.nextSeq,
		stream: stream,
		data:   append([]byte(nil), data...),
	})
	b.total += int64(len(data))
	b.nextSeq += int64(len(data))
	b.trimLocked()
	b.wakeReadersLocked()
}

// finish marks the stream complete and records the exit outcome.
func (b *outputBuffer) finish(exit sandbox.SessionExit, waitErr error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.eof {
		return
	}
	b.eof = true
	b.exit = exit
	b.waitErr = waitErr
	b.wakeReadersLocked()
}

func (b *outputBuffer) read(ctx context.Context, afterSeq int64, maxBytes int) (sandbox.SessionOutput, error) {
	if maxBytes <= 0 {
		return sandbox.SessionOutput{}, errdefs.Validationf("windows/elevated: Read maxBytes must be positive")
	}
	b.mu.Lock()
	for {
		if retained := b.retainedSeqLocked(); retained > afterSeq {
			b.mu.Unlock()
			return sandbox.SessionOutput{}, fmt.Errorf("%w: afterSeq %d, retained from %d", sandbox.ErrSequenceGap, afterSeq, retained)
		}
		if b.nextSeq < afterSeq {
			b.mu.Unlock()
			return sandbox.SessionOutput{}, errdefs.Validationf(
				"windows/elevated: afterSeq %d is beyond buffered output (next=%d)", afterSeq, b.nextSeq)
		}
		if out, ok := b.collectLocked(afterSeq, maxBytes); ok {
			b.mu.Unlock()
			return out, nil
		}
		if b.eof {
			b.mu.Unlock()
			return sandbox.SessionOutput{NextSeq: afterSeq, EOF: true}, nil
		}
		wake := b.wake
		b.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return sandbox.SessionOutput{}, ctx.Err()
		}
		b.mu.Lock()
	}
}

func (b *outputBuffer) collectLocked(afterSeq int64, maxBytes int) (sandbox.SessionOutput, bool) {
	remaining := int64(maxBytes)
	next := afterSeq
	var chunks []sandbox.OutputChunk
	for _, ch := range b.chunks {
		if remaining <= 0 {
			break
		}
		end := ch.seq + int64(len(ch.data))
		if next >= end {
			continue
		}
		start := next - ch.seq
		n := int64(len(ch.data)) - start
		if n > remaining {
			n = remaining
		}
		chunks = append(chunks, sandbox.OutputChunk{
			Seq:    next,
			Stream: ch.stream,
			Data:   append([]byte(nil), ch.data[start:start+n]...),
		})
		next += n
		remaining -= n
	}
	if len(chunks) == 0 {
		return sandbox.SessionOutput{}, false
	}
	return sandbox.SessionOutput{
		NextSeq: next,
		Chunks:  chunks,
		EOF:     b.eof && next == b.nextSeq,
	}, true
}

func (b *outputBuffer) retainedSeqLocked() int64 {
	if len(b.chunks) == 0 {
		return b.nextSeq
	}
	return b.chunks[0].seq
}

func (b *outputBuffer) trimLocked() {
	if b.max <= 0 {
		return
	}
	for len(b.chunks) > 1 && b.total > b.max {
		b.total -= int64(len(b.chunks[0].data))
		b.chunks = b.chunks[1:]
	}
}

func (b *outputBuffer) wakeReadersLocked() {
	close(b.wake)
	b.wake = make(chan struct{})
}

// exitLocked returns the recorded outcome (zero values while running).
func (b *outputBuffer) exitLocked() (sandbox.SessionExit, error) {
	return b.exit, b.waitErr
}
