// Package fsatomic publishes a file atomically: the payload goes to a
// temp file in the destination directory, optionally fsynced, then
// renamed over the target. A crash mid-write therefore never leaves a
// truncated file at the destination.
//
// It exists because the same temp-file dance kept being hand-rolled
// with slightly different options at each call site. The options below
// are exactly those differences, not a redesign: callers keep the
// MkdirAll/perm/sync behaviour they had, and the failure paths (temp
// removed, single telemetry message) live here once.
package fsatomic

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Options carries what the call sites actually differ in. The zero
// value is meaningful: no chmod, no mkdir, no fsync, ".tmp-*" prefix.
type Options struct {
	// Perm is the final permission of the published file. Zero keeps
	// the temp file's own mode (0600 from os.CreateTemp).
	Perm os.FileMode
	// MkdirPerm, when non-zero, creates the parent directory with this
	// mode before writing.
	MkdirPerm os.FileMode
	// TempPrefix is the os.CreateTemp pattern; empty means ".tmp-*".
	TempPrefix string
	// Sync fsyncs the temp file before the rename. Callers that need
	// the durability keep it; the difference stays per call site
	// instead of being silently unified.
	Sync bool
}

// Write publishes data at path: same-directory temp file → chmod →
// write → (sync) → close → rename. On every failure path the temp file
// is removed (one telemetry warning if the removal itself fails) and
// the destination is left untouched.
func Write(path string, data []byte, opts Options) error {
	dir := filepath.Dir(path)
	if opts.MkdirPerm != 0 {
		if err := os.MkdirAll(dir, opts.MkdirPerm); err != nil {
			return fmt.Errorf("fsatomic: create directory: %w", err)
		}
	}
	pattern := opts.TempPrefix
	if pattern == "" {
		pattern = ".tmp-*"
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("fsatomic: create temp file: %w", err)
	}
	name := tmp.Name()
	// No-op after a successful rename (the name no longer exists).
	defer func() {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			telemetry.WarnErr(context.Background(),
				"fsatomic: remove temp after failure", err)
		}
	}()
	if opts.Perm != 0 {
		if err := tmp.Chmod(opts.Perm); err != nil {
			return closeAfter(tmp, "chmod", err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return closeAfter(tmp, "write", err)
	}
	if opts.Sync {
		if err := tmp.Sync(); err != nil {
			return closeAfter(tmp, "sync", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fsatomic: close temp file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("fsatomic: rename temp file: %w", err)
	}
	return nil
}

// closeAfter closes the temp file on a failure path where the caller
// still holds it open, and wraps the original cause.
func closeAfter(tmp *os.File, op string, cause error) error {
	telemetry.WarnErr(context.Background(),
		"fsatomic: close temp after "+op+" failure", tmp.Close())
	return fmt.Errorf("fsatomic: %s temp file: %w", op, cause)
}
