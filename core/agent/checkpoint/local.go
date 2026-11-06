// Package checkpoint stores agent checkpoints on the local filesystem.
//
// The layout is one append-only log, one pointer, and a content-addressed blob
// directory:
//
//	log.jsonl        one JSON Checkpoint per line, appended, never rewritten
//	pointer.json     offset and length of the newest line
//	blobs/<ab>/<sha> artifact bytes, written once per content
//
// Appending keeps the cost of a round proportional to what that round added;
// a full snapshot every round would rewrite the whole transcript and make a
// long conversation quadratic. The pointer is written last, so a reader always
// sees either the previous valid checkpoint or the new one, never a torn line.
package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

const (
	logName     = "log.jsonl"
	pointerName = "pointer.json"
	blobDirName = "blobs"
)

// pointer locates the newest checkpoint in the log.
type pointer struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// Local is a filesystem checkpoint store.
type Local struct {
	dir string
}

var _ agent.Store = (*Local)(nil)

// New prepares a store rooted at dir. The directory is created when missing.
func New(dir string) (*Local, error) {
	if dir == "" {
		return nil, errdefs.Validationf("checkpoint: a directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	return &Local{dir: dir}, nil
}

// Write appends one checkpoint and advances the pointer.
func (l *Local) Write(ctx context.Context, checkpoint agent.Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("checkpoint: encode: %w", err)
	}
	body = append(body, '\n')

	log, err := os.OpenFile(filepath.Join(l.dir, logName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("checkpoint: open log: %w", err)
	}
	defer func() { _ = log.Close() }()
	info, err := log.Stat()
	if err != nil {
		return fmt.Errorf("checkpoint: stat log: %w", err)
	}
	offset := info.Size()
	if _, err := log.Write(body); err != nil {
		return fmt.Errorf("checkpoint: append: %w", err)
	}
	// One sync per checkpoint is the durability point; the pointer below must
	// never name bytes the log has not flushed.
	if err := log.Sync(); err != nil {
		return fmt.Errorf("checkpoint: sync log: %w", err)
	}
	return l.writePointer(pointer{Offset: offset, Length: int64(len(body))})
}

// Latest returns the newest checkpoint.
func (l *Local) Latest(ctx context.Context) (agent.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return agent.Checkpoint{}, err
	}
	body, err := os.ReadFile(filepath.Join(l.dir, pointerName))
	if errors.Is(err, os.ErrNotExist) {
		return agent.Checkpoint{}, agent.ErrNoCheckpoint
	}
	if err != nil {
		return agent.Checkpoint{}, fmt.Errorf("checkpoint: read pointer: %w", err)
	}
	var mark pointer
	if err := json.Unmarshal(body, &mark); err != nil {
		return agent.Checkpoint{}, fmt.Errorf("checkpoint: decode pointer: %w", err)
	}
	log, err := os.Open(filepath.Join(l.dir, logName))
	if err != nil {
		return agent.Checkpoint{}, fmt.Errorf("checkpoint: open log: %w", err)
	}
	defer func() { _ = log.Close() }()
	buf := make([]byte, mark.Length)
	if _, err := log.ReadAt(buf, mark.Offset); err != nil {
		return agent.Checkpoint{}, fmt.Errorf("checkpoint: read entry: %w", err)
	}
	var checkpoint agent.Checkpoint
	if err := json.Unmarshal(buf, &checkpoint); err != nil {
		return agent.Checkpoint{}, fmt.Errorf("checkpoint: decode entry: %w", err)
	}
	return checkpoint, nil
}

// PutBlob stores bytes under their content hash. Storing the same bytes twice
// costs a stat, which is what makes repeated tool output cheap.
func (l *Local) PutBlob(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	ref := hex.EncodeToString(sum[:])
	path := l.blobPath(ref)
	if _, err := os.Stat(path); err == nil {
		return ref, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("checkpoint: blob dir: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return "", fmt.Errorf("checkpoint: blob temp: %w", err)
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("checkpoint: blob write: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("checkpoint: blob close: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("checkpoint: blob rename: %w", err)
	}
	return ref, nil
}

// GetBlob returns the bytes a reference names.
func (l *Local) GetBlob(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref) != sha256.Size*2 {
		return nil, errdefs.Validationf("checkpoint: %q is not a blob reference", ref)
	}
	body, err := os.ReadFile(l.blobPath(ref))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read blob: %w", err)
	}
	return body, nil
}

func (l *Local) blobPath(ref string) string {
	return filepath.Join(l.dir, blobDirName, ref[:2], ref)
}

func (l *Local) writePointer(mark pointer) error {
	body, err := json.Marshal(mark)
	if err != nil {
		return fmt.Errorf("checkpoint: encode pointer: %w", err)
	}
	path := filepath.Join(l.dir, pointerName)
	temp := path + ".tmp"
	if err := os.WriteFile(temp, body, 0o644); err != nil {
		return fmt.Errorf("checkpoint: write pointer: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("checkpoint: rename pointer: %w", err)
	}
	return nil
}
