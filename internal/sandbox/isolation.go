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
	backendBubblewrap isolationBackend = iota // Linux Bubblewrap namespace isolation
)

func (b isolationBackend) String() string {
	return "bubblewrap"
}

// probeResult holds the outcome of an isolation probe.
type probeResult struct {
	backend   isolationBackend
	bwrapPath string // absolute path to bwrap binary (empty when bare)
}

// probeIsolation detects the available isolation backend.
// Requires Linux with Bubblewrap installed. Returns an error on non-Linux
// platforms or when bwrap is missing.
func probeIsolation() (probeResult, error) {
	if runtime.GOOS != "linux" {
		return probeResult{}, fmt.Errorf("sandbox: requires Linux (current OS: %s)", runtime.GOOS)
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
