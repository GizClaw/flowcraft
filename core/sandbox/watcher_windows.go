//go:build windows

package sandbox

// groupCapsAvailable is false on Windows for the shared sampler: it
// relies on ps(1) process-group accounting, which does not exist here.
// The Windows backend (core/sandbox/windows) gates MemoryCap/CPUCap on
// its own job-object probe instead, so callers of the shared
// ValidateExecPolicy still get an honest rejection rather than a
// silently unenforced cap.
func groupCapsAvailable() bool { return false }
