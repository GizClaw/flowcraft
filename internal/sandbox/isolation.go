package sandbox

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

// isolationBackend represents the isolation method used by LocalSandbox.
type isolationBackend int

const (
	backendBare       isolationBackend = iota // bare process, no isolation
	backendBubblewrap                         // Linux Bubblewrap namespace isolation
)

func (b isolationBackend) String() string {
	switch b {
	case backendBubblewrap:
		return "bubblewrap"
	default:
		return "bare"
	}
}

// probeResult holds the outcome of an isolation probe.
type probeResult struct {
	backend   isolationBackend
	bwrapPath string // absolute path to bwrap binary (empty when bare)
}

// probeIsolation detects the best available isolation backend.
// Three-layer validation: OS → LookPath → smoke test.
// Any layer failure degrades to bare. Idempotent and side-effect free.
func probeIsolation() probeResult {
	if runtime.GOOS != "linux" {
		return probeResult{backend: backendBare}
	}

	p, err := exec.LookPath("bwrap")
	if err != nil {
		return probeResult{backend: backendBare}
	}

	// Smoke test: verify bwrap can actually create namespaces.
	// Covers: container without CAP_SYS_ADMIN, kernel disabling unprivileged userns, etc.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, p,
		"--ro-bind", "/", "/",
		"--unshare-user",
		"--unshare-pid",
		"--die-with-parent",
		"--", "true",
	).Run(); err != nil {
		return probeResult{backend: backendBare}
	}

	return probeResult{backend: backendBubblewrap, bwrapPath: p}
}
