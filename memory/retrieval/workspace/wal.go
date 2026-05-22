package workspace

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// walMagic is the 4-byte ASCII tag at the head of every WAL log
// file. A file that does not begin with this magic is rejected on
// replay rather than parsed as records, which prevents a misrouted
// segment file (or any other foreign content) from being silently
// applied to the memtable.
var walMagic = [4]byte{'F', 'C', 'W', 'L'}

// walHeaderSize is the on-disk size of [walHeader] in bytes:
// 4 magic + 1 version + 3 reserved.
const walHeaderSize = 8

// walRecordLengthSize is the size of the uint32 length prefix that
// frames every record. Total record bytes on disk are
// walRecordLengthSize + len(payload).
const walRecordLengthSize = 4

// walWriter appends records to one active log under <ns>/wal/, with
// rotation when the log crosses [Config.walMaxBytes]. Methods are
// goroutine-safe; calls are serialised by the namespace-level rwMu
// in practice but the writer holds its own mutex for clarity.
type walWriter struct {
	ws       sdkworkspace.Workspace
	paths    pathHelper
	maxBytes int

	mu         sync.Mutex
	currentSeq uint64 // 0 means "no active log; next Append opens one"
	currentLen int    // bytes written to currentSeq, including header
}

// newWALWriter prepares a writer that will allocate sequence numbers
// starting from lastSeq+1. lastSeq comes from the loaded manifest
// (or 0 for a fresh namespace).
func newWALWriter(ws sdkworkspace.Workspace, paths pathHelper, maxBytes int, lastSeq uint64) *walWriter {
	return &walWriter{
		ws:         ws,
		paths:      paths,
		maxBytes:   maxBytes,
		currentSeq: lastSeq, // pre-increment in openNew()
	}
}

// CurrentSeq returns the active log sequence (0 if none).
func (w *walWriter) CurrentSeq() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.currentSeq
}

// Append serialises one record to the active log, rotating into a
// fresh log file when [Config.walMaxBytes] would be exceeded. Each
// successful Append's bytes are durable to whatever degree the
// underlying Workspace guarantees (LocalWorkspace -> POSIX page
// cache; MemWorkspace -> volatile).
func (w *walWriter) Append(ctx context.Context, rec walRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("walWriter: marshal: %w", err)
	}
	frame := make([]byte, walRecordLengthSize+len(payload))
	binary.BigEndian.PutUint32(frame[:walRecordLengthSize], uint32(len(payload)))
	copy(frame[walRecordLengthSize:], payload)

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.currentSeq == 0 {
		if err := w.openNewLocked(ctx); err != nil {
			return err
		}
	}

	if w.maxBytes > 0 && w.currentLen+len(frame) > w.maxBytes {
		if err := w.openNewLocked(ctx); err != nil {
			return err
		}
	}

	if err := w.ws.Append(ctx, w.paths.walPath(w.currentSeq), frame); err != nil {
		return fmt.Errorf("walWriter: append seq=%d: %w", w.currentSeq, err)
	}
	w.currentLen += len(frame)
	return nil
}

// openNewLocked allocates the next sequence number and writes a
// fresh log header. Caller holds w.mu.
func (w *walWriter) openNewLocked(ctx context.Context) error {
	w.currentSeq++
	hdr := make([]byte, walHeaderSize)
	copy(hdr[:4], walMagic[:])
	hdr[4] = walRecordVersion
	if err := w.ws.Write(ctx, w.paths.walPath(w.currentSeq), hdr); err != nil {
		return fmt.Errorf("walWriter: open log seq=%d: %w", w.currentSeq, err)
	}
	w.currentLen = walHeaderSize
	return nil
}

// Rotate closes the active log and opens a fresh one with the next
// sequence number. Useful before a flush so the flushed memtable's
// records all live in a self-contained log that can be retired
// after manifest swap.
func (w *walWriter) Rotate(ctx context.Context) (oldSeq uint64, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	oldSeq = w.currentSeq
	if oldSeq == 0 {
		return 0, nil
	}
	if err := w.openNewLocked(ctx); err != nil {
		return oldSeq, err
	}
	return oldSeq, nil
}

// Close releases the writer. The current log file is left in place;
// recovery on next open will replay its records.
func (w *walWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.currentSeq = 0
	w.currentLen = 0
	return nil
}

// listWALSeqs scans <ns>/wal/ and returns the sorted list of
// sequence numbers it finds. Files with names that do not match the
// expected hex layout are ignored — they are most likely partial
// writes or operator detritus, not WAL logs.
func listWALSeqs(ctx context.Context, ws sdkworkspace.Workspace, paths pathHelper) ([]uint64, error) {
	entries, err := ws.List(ctx, paths.walDir())
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("walReader: list: %w", err)
	}
	out := make([]uint64, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		stem := strings.TrimSuffix(name, ".log")
		seq, err := strconv.ParseUint(stem, 16, 64)
		if err != nil {
			continue
		}
		out = append(out, seq)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// replayWAL reads every WAL log with seq >= firstUnconsumed and
// applies its records to dst. A truncated tail (length prefix
// exceeds remaining bytes, or JSON unmarshal fails) is treated as
// end-of-log and the preceding records are kept. A header
// mismatch is fatal and surfaces as [ErrCorrupt] so the operator
// notices file-system damage instead of silently degrading the
// index.
//
// Returns the highest seq examined (regardless of how many records
// it actually contained) so the caller can update the active
// writer's sequence counter accordingly.
func replayWAL(
	ctx context.Context,
	ws sdkworkspace.Workspace,
	paths pathHelper,
	firstUnconsumed uint64,
	dst *memtable,
) (highestSeq uint64, err error) {
	seqs, err := listWALSeqs(ctx, ws, paths)
	if err != nil {
		return 0, err
	}
	for _, seq := range seqs {
		if seq < firstUnconsumed {
			continue
		}
		highestSeq = seq
		data, err := ws.Read(ctx, paths.walPath(seq))
		if err != nil {
			if errors.Is(err, sdkworkspace.ErrNotFound) {
				continue
			}
			return highestSeq, fmt.Errorf("walReader: read seq=%d: %w", seq, err)
		}
		if err := replayWALBytes(data, dst); err != nil {
			if errors.Is(err, ErrCorrupt) {
				return highestSeq, fmt.Errorf("walReader: seq=%d: %w", seq, err)
			}
			// Record-level truncation tail: stop on this log,
			// keep records already applied.
			return highestSeq, nil
		}
	}
	return highestSeq, nil
}

// replayWALBytes parses one WAL log's bytes. Returns nil on success
// (clean tail), nil on truncated tail (preserves applied prefix),
// [ErrCorrupt] on header mismatch.
func replayWALBytes(data []byte, dst *memtable) error {
	if len(data) < walHeaderSize {
		return ErrCorrupt
	}
	if [4]byte{data[0], data[1], data[2], data[3]} != walMagic {
		return ErrCorrupt
	}
	if data[4] != walRecordVersion {
		return ErrCorrupt
	}
	off := walHeaderSize
	for off < len(data) {
		if off+walRecordLengthSize > len(data) {
			return errTruncatedTail
		}
		ln := binary.BigEndian.Uint32(data[off : off+walRecordLengthSize])
		off += walRecordLengthSize
		if ln == 0 || off+int(ln) > len(data) {
			return errTruncatedTail
		}
		var rec walRecord
		if err := json.Unmarshal(data[off:off+int(ln)], &rec); err != nil {
			return errTruncatedTail
		}
		off += int(ln)
		dst.applyWALRecord(rec)
	}
	return nil
}

// errTruncatedTail signals that a WAL log ended mid-record. The
// caller treats it as a clean stop, not corruption, because a power
// loss between two records is normal for an append-only log.
var errTruncatedTail = errors.New("walReader: truncated tail")
