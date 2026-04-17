//go:build windows

package machine

func newWindows(version string) Machine {
	return NewWSL(version)
}
