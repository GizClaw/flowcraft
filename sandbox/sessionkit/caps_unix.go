//go:build unix

package sessionkit

import (
	"math"
	"time"

	"github.com/GizClaw/flowcraft/core/policy"
)

// deriveGroupCaps converts policy resource limits into group-watcher
// units. It has no windows caller: the windows backend watches a job object, which
// carries the limits itself, so a shared file here would be a function the windows
// build cannot reach. Memory: bytes -> KiB of aggregate group RSS. CPU: millicores
// scaled by the per-call timeout -> a cpu-time budget for the whole
// group (a cap on cumulative cpu time, not rate), so CPUMillicores is
// only actionable together with Timeout — the caller-visible guard for
// that lives in Exec.
func deriveGroupCaps(res policy.Resources, timeout time.Duration) (maxRSSKB int64, maxCPU time.Duration) {
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
