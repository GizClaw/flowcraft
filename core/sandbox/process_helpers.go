package sandbox

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// deriveGroupCaps converts policy resource limits into group-watcher
// units. Memory: bytes -> KiB of aggregate group RSS. CPU: millicores
// scaled by the per-call timeout -> a cpu-time budget for the whole
// group (a cap on cumulative cpu time, not rate), so CPUMillicores is
// only actionable together with Timeout — the caller-visible guard for
// that lives in Exec.
func deriveGroupCaps(res ResourceLimits, timeout time.Duration) (maxRSSKB int64, maxCPU time.Duration) {
	if res.MemoryBytes > 0 {
		maxRSSKB = 1 + (res.MemoryBytes-1)/1024
	}
	if res.CPUMillicores > 0 && timeout > 0 {
		scaled := float64(timeout) * float64(res.CPUMillicores) / 1000
		if scaled >= float64(math.MaxInt64) {
			maxCPU = time.Duration(math.MaxInt64)
		} else {
			maxCPU = time.Duration(scaled)
		}
	}
	return maxRSSKB, maxCPU
}

// classifyStartError maps process-start failures onto errdefs
// categories: a missing binary is NotFound, a permission refusal is
// Forbidden, anything else is Internal. Callers (e.g. script bridges)
// rely on these categories instead of string-matching os/exec errors.
func classifyStartError(cmd string, err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errdefs.NotFound(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	case errors.Is(err, os.ErrPermission):
		return errdefs.Forbidden(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	default:
		return errdefs.Internal(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	}
}
