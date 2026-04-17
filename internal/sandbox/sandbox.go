// Package sandbox provides isolated execution environments for code.
// Implementations include Docker containers (full isolation) and local
// OS processes (dev/test degradation).
package sandbox

import (
	"context"
	"errors"
	"time"
)

// Sandbox provides an isolated execution environment.
// Implementations must be safe for concurrent use.
type Sandbox interface {
	ID() string
	Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
	Close() error
}

// Mode controls sandbox lifecycle behavior.
type Mode string

const (
	ModeEphemeral  Mode = "ephemeral"
	ModeSession    Mode = "session"
	ModePersistent Mode = "persistent"
)

// ParseMode converts a string to Mode, defaulting to ModeSession.
func ParseMode(s string) Mode {
	switch Mode(s) {
	case ModeEphemeral, ModeSession, ModePersistent:
		return Mode(s)
	default:
		return ModeSession
	}
}

// ExecOptions configures a command execution.
type ExecOptions struct {
	WorkDir string            `json:"work_dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Stdin   []byte            `json:"-"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

// ExecResult captures command output.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// AcquireOptions configures sandbox creation via Manager.Acquire.
type AcquireOptions struct {
	Mode        Mode          `json:"mode,omitempty"`
	IdleTimeout time.Duration `json:"idle_timeout,omitempty"`
}

// Common errors.
var (
	ErrClosed        = errors.New("sandbox: closed")
	ErrPathTraversal = errors.New("sandbox: path traversal denied")
	ErrNotFound      = errors.New("sandbox: file not found")
	ErrLimitReached  = errors.New("sandbox: max concurrent sandboxes reached")
)
