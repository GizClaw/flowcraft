//go:build !windows

package machine

func newWindows(version string) Machine {
	_ = version
	return NewNative()
}
