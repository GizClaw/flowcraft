package machine

import "runtime"

// Resolve returns the appropriate Machine implementation for the current OS.
// On Linux it returns a native process manager; on macOS a Lima VM manager;
// on Windows a WSL manager. Other platforms get a stub that returns errors.
func Resolve(version string) Machine {
	switch runtime.GOOS {
	case "linux":
		return NewNative()
	case "darwin":
		return newDarwin(version)
	case "windows":
		return newWindows(version)
	default:
		return NewNative()
	}
}
