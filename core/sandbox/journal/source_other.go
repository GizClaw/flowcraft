//go:build !linux && !darwin && !windows

package journal

import (
	"runtime"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// available is false here on purpose. The engine, the folding rules and
// the gap accounting are all platform-neutral and compile everywhere;
// only the watch source is not. Linux has inotify, macOS has kqueue and
// Windows has ReadDirectoryChangesW (see source_linux.go,
// source_darwin.go and source_windows.go); a platform without one — the
// BSDs today, or a GOOS nobody has written a source for — reports
// sandbox.JournalCapabilities.Enabled = false rather than pretending to
// watch.
func available() bool { return false }

// defaultBudget is zero: without a source there is nothing to budget.
func defaultBudget() int { return 0 }

func openSource() (Source, error) {
	return nil, errdefs.NotAvailablef(
		"sandbox/journal: no file-watch source for %s; inotify and kqueue are the ones that exist", runtime.GOOS)
}
