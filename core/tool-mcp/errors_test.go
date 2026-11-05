package mcp

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestCannotStart covers the spawn failures a retry cannot fix and the
// ones it can: only the "this command will never run" class may be
// treated as terminal by the retry loop.
func TestCannotStart(t *testing.T) {
	t.Parallel()
	missingPath := &fs.PathError{
		Op: "fork/exec", Path: "/x/server", Err: syscall.ENOENT,
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "missing command file",
			err:  missingPath,
			want: true,
		}, {
			name: "wrapped missing command file",
			err:  fmt.Errorf("connect: %w", missingPath),
			want: true,
		}, {
			name: "command file without the exec bit",
			err: &fs.PathError{
				Op: "fork/exec", Path: "/x/server", Err: syscall.EACCES,
			},
			want: true,
		}, {
			name: "command file the OS cannot run",
			err: &fs.PathError{
				Op: "fork/exec", Path: "/x/server", Err: syscall.ENOEXEC,
			},
			want: true,
		}, {
			name: "bare command missing from PATH",
			err:  &exec.Error{Name: "nope", Err: exec.ErrNotFound},
			want: true,
		}, {
			name: "bare command that is not executable",
			err:  &exec.Error{Name: "nope", Err: fs.ErrPermission},
			want: true,
		}, {
			name: "fork refused for lack of resources",
			err: &fs.PathError{
				Op: "fork/exec", Path: "/x/server", Err: syscall.EAGAIN,
			},
			want: false,
		}, {
			name: "refused dial",
			err:  errors.New("connection refused"),
			want: false,
		}, {
			name: "child that exited during startup",
			err:  errors.New("exit status 1"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cannotStart(tc.err); got != tc.want {
				t.Fatalf("cannotStart(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestConnectErrorClassifiesUnrunnableCommands pins the marker the
// retry loop branches on: a command the OS cannot start is Validation
// (terminal, and a waiting host is woken with it), while a spawn
// failure a retry may fix stays NotAvailable.
func TestConnectErrorClassifiesUnrunnableCommands(t *testing.T) {
	t.Parallel()
	missing := connectError("broken", &fs.PathError{
		Op: "fork/exec", Path: "/x/server", Err: syscall.ENOENT,
	})
	if !errdefs.IsValidation(missing) || errdefs.IsNotAvailable(missing) {
		t.Fatalf("missing command classified as %v, want Validation", missing)
	}
	if !strings.Contains(missing.Error(), "cannot start command") {
		t.Fatalf("error %q does not name the failed start", missing)
	}

	busy := connectError("busy", &fs.PathError{
		Op: "fork/exec", Path: "/x/server", Err: syscall.EAGAIN,
	})
	if !errdefs.IsNotAvailable(busy) || errdefs.IsValidation(busy) {
		t.Fatalf("resource failure classified as %v, want NotAvailable", busy)
	}
}

// TestCannotStartRealExecErrors runs the predicate against the errors
// exec actually produces, so the classification cannot drift away from
// the OS error shapes it is meant to match.
func TestCannotStartRealExecErrors(t *testing.T) {
	t.Parallel()
	if err := exec.Command("/definitely/not/here/mcp-server").Start(); err == nil || !cannotStart(err) {
		t.Fatalf("missing absolute command: err=%v cannotStart=%v, want true",
			err, cannotStart(err))
	}
	if _, err := exec.LookPath("definitely-not-a-real-command-xyz"); err == nil || !cannotStart(err) {
		t.Fatalf("missing PATH command: err=%v cannotStart=%v, want true",
			err, cannotStart(err))
	}
}
