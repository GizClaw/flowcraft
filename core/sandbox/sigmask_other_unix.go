//go:build unix && !linux && !darwin

package sandbox

import "github.com/GizClaw/flowcraft/core/errdefs"

// sigset is opaque here: this platform has no mask helper, so nothing
// ever fills one in. The type exists so the spawn path stays free of
// per-GOOS branches.
type sigset struct{}

func emptySigset() sigset { return sigset{} }

// replaceThreadSignalMask reports that this platform exposes no mask API,
// which leaves startWithCleanSignalMask spawning on whatever thread it is
// given - a child here keeps the mask of the thread that forked it.
func replaceThreadSignalMask(sigset) (sigset, error) {
	return sigset{}, errdefs.NotAvailablef(
		"sandbox: signal masks are not exposed on this platform")
}
