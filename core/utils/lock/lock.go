package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Info describes one lock holder. The holder writes it once the kernel
// lock is taken, which is what lets a rejected launch name the owner of
// the file instead of reporting a bare conflict.
type Info struct {
	// PID is the holder's process id.
	PID int `json:"pid"`
	// Kind names the surface holding the lock ("manager", "host", "gui").
	Kind string `json:"kind,omitempty"`
	// Started is when the holder took the lock (RFC3339, UTC).
	Started string `json:"started,omitempty"`
	// Version is the holder's build version, empty when the binary
	// carries none.
	Version string `json:"version,omitempty"`
	// Endpoint is an optional address at which the holder can be reached
	// (to raise its window, to accept forwarded arguments). This package
	// only records the value and hands it back with a HeldError; the
	// protocol behind the address belongs to the caller.
	Endpoint string `json:"endpoint,omitempty"`
}

// LiveFrom reports whether the holder recorded its own liveness window
// starting at or before t. It is a convenience for diagnostics: the
// authoritative liveness signal is the kernel lock, never this record.
func (i Info) LiveFrom(t time.Time) bool {
	if i.Started == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, i.Started)
	if err != nil {
		return false
	}
	return !started.After(t)
}

// Handle is a held lock. Release it, or let the process die, to hand the
// file to the next process: the kernel drops the lock either way.
type Handle struct {
	path   string
	info   Info
	file   *os.File
	shared bool
}

// Path is the lock file this handle holds.
func (h *Handle) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// Info names the holder this handle recorded — its own process, unless
// Owned reports false.
func (h *Handle) Info() Info {
	if h == nil {
		return Info{}
	}
	return h.info
}

// Owned reports whether this handle owns the kernel lock. It is false
// for a handle that ShareWithinProcess handed to a second caller of the
// same process: the lock is effectively ours either way, but only the
// owning handle releases it.
func (h *Handle) Owned() bool {
	return h != nil && !h.shared && h.file != nil
}

// Release drops the lock. The lock file stays behind with the last
// holder's record, which is what makes a later "who held this" question
// answerable; only the kernel lock decides liveness. Release on a shared
// handle is a no-op.
func (h *Handle) Release() error {
	if h == nil || h.file == nil {
		return nil
	}
	err := unlockFile(h.file)
	closeErr := h.file.Close()
	h.file = nil
	return errors.Join(err, closeErr)
}

// HeldError reports that another live holder owns the lock.
type HeldError struct {
	// Path is the lock file.
	Path string
	// Info is the holder as recorded in the file (zero values when the
	// file holds no record this package can read).
	Info Info
}

func (e *HeldError) Error() string {
	if e.Info.PID == 0 {
		return fmt.Sprintf("lock: %s is held by another live process", e.Path)
	}
	who := fmt.Sprintf("pid %d", e.Info.PID)
	if e.Info.Kind != "" {
		who += " (" + e.Info.Kind + ")"
	}
	if e.Info.Started != "" {
		who += " since " + e.Info.Started
	}
	return fmt.Sprintf("lock: %s is held by %s", e.Path, who)
}

// ErrHeld is the sentinel behind every HeldError.
var ErrHeld = errors.New("lock: held by another live process")

func (e *HeldError) Unwrap() error { return ErrHeld }

// IsHeld reports whether err means "another live holder" and returns that
// holder. It walks wrapped errors, so a caller that wraps Acquire's
// failure in its own conflict error still reaches the holder record.
func IsHeld(err error) (Info, bool) {
	var held *HeldError
	if errors.As(err, &held) {
		return held.Info, true
	}
	return Info{}, false
}

// Acquire takes the lock on path without blocking.
//
//   - (handle, nil): this process holds the lock. A handle whose Owned
//     reports false was handed to a second caller of this same process
//     under ShareWithinProcess; its Release is a no-op.
//   - (nil, err with IsHeld(err)): another live holder. Refuse.
//   - (nil, err): the lock could not be taken, or the holder record could
//     not be written. See the package doc for the fail-open /
//     fail-closed rule.
//
// ctx only carries the diagnostic log of a close that failed after the
// lock decision was already made: the acquisition never waits, so there
// is nothing for it to cancel.
func Acquire(ctx context.Context, path, kind string, opts ...Option) (*Handle, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("lock: lock path is empty")
	}
	var cfg config
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("lock: create lock dir: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: open lock file: %w", err)
	}
	if err := lockFile(file); err != nil {
		holder := readInfo(path)
		if isLockBusy(err) {
			closeQuiet(ctx, file)
			if holder.PID == os.Getpid() && cfg.shareWithinProcess {
				return &Handle{path: path, info: holder, shared: true}, nil
			}
			return nil, &HeldError{Path: path, Info: holder}
		}
		return nil, errors.Join(
			fmt.Errorf("lock: lock %s: %w", path, err), closeLocked(file))
	}
	info := Info{
		PID:      os.Getpid(),
		Kind:     kind,
		Started:  time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Version:  cfg.version,
		Endpoint: cfg.endpoint,
	}
	if info.Version == "" {
		info.Version = buildVersion()
	}
	if err := writeInfo(file, info); err != nil {
		return nil, errors.Join(
			fmt.Errorf("lock: record holder: %w", err), closeLocked(file))
	}
	return &Handle{path: path, info: info, file: file}, nil
}

// ReadInfo returns the last holder recorded in a lock file, if any. It
// reads the file only: the result says nothing about liveness, and a
// record in a format this package does not know (an older release left a
// bare pid line) reports false while the lock stays acquirable.
func ReadInfo(path string) (Info, bool) {
	info := readInfo(path)
	return info, info.PID != 0
}

// readInfo decodes the holder record, tolerating a truncated or
// half-written file: the writer holds the lock, so a reader that lost
// the race sees the previous record or nothing.
func readInfo(path string) Info {
	if path == "" {
		return Info{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Info{}
	}
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return Info{}
	}
	return info
}

// writeInfo records the holder and makes it visible to the next process
// that finds the lock busy.
func writeInfo(file *os.File, info Info) error {
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

// buildVersion reports the running module's build version, or "" when the
// binary carries none (go run, tests). It is what the holder record falls
// back to, so a user binary names its app build without passing anything.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	version := strings.TrimSpace(info.Main.Version)
	if version == "(devel)" {
		return ""
	}
	return version
}

// closeLocked closes a lock file descriptor and hands its error back, so
// callers that are already returning one can carry it.
func closeLocked(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

// closeQuiet closes a descriptor whose failure cannot change the caller's
// answer: the lock decision has been made, and the kernel lock — not the
// descriptor — is what decides who owns the file. A genuinely surprising
// failure is recorded, not dropped.
func closeQuiet(ctx context.Context, file *os.File) {
	if file == nil {
		return
	}
	if err := file.Close(); err != nil {
		telemetry.WarnErr(ctx, "lock: close lock file failed", err)
	}
}

// config is the resolved option set of one Acquire.
type config struct {
	shareWithinProcess bool
	version            string
	endpoint           string
}

// Option configures one Acquire.
type Option func(*config)

// ShareWithinProcess lets a second Acquire of one path inside one process
// succeed as a shared handle instead of reporting the file as held. Two
// components of one process do legitimately address one state root (an
// embedded host, tests), and they must not deadlock each other. The
// shared handle is Owned() == false and its Release is a no-op: the first
// handle owns the kernel lock, and the file stays locked until it lets go.
//
// The default is strict, which is what a single-instance lock wants: a
// second holder inside the same process is still a second instance.
//
// The same-process case is detected from the pid in the holder record, so
// two processes that report one pid (containers, pid namespaces) only
// ever get the shared answer, never a corrupted one.
func ShareWithinProcess() Option {
	return func(c *config) { c.shareWithinProcess = true }
}

// WithVersion records version as the holder's build version
// (Info.Version). Without it, the record carries the build version of the
// running module when the binary has one.
func WithVersion(version string) Option {
	return func(c *config) { c.version = version }
}

// WithEndpoint records endpoint in the holder record (Info.Endpoint): an
// address a rejected launch can use to reach the holder and ask it to
// come to the front. This package only stores the value; the protocol
// behind the address belongs to the caller.
func WithEndpoint(endpoint string) Option {
	return func(c *config) { c.endpoint = endpoint }
}
