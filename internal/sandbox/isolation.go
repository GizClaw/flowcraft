package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// isolationBackend represents the isolation method used by LocalSandbox.
type isolationBackend int

const (
	backendBare       isolationBackend = iota // non-Linux dev/test only
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
// On Linux, bwrap is required — missing bwrap returns an error instead of
// silently degrading to bare execution. On non-Linux (dev/test only),
// bare execution is used.
func probeIsolation() (probeResult, error) {
	if runtime.GOOS != "linux" {
		return probeResult{backend: backendBare}, nil
	}

	p, err := exec.LookPath("bwrap")
	if err != nil {
		return probeResult{}, fmt.Errorf("sandbox: bwrap not found; install bubblewrap for sandbox isolation")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, p,
		"--ro-bind", "/", "/",
		"--unshare-user",
		"--unshare-pid",
		"--die-with-parent",
		"--", "true",
	).Run(); err != nil {
		return probeResult{}, fmt.Errorf("sandbox: bwrap smoke test failed: %w", err)
	}

	return probeResult{backend: backendBubblewrap, bwrapPath: p}, nil
}
