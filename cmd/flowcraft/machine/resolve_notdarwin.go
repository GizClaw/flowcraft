//go:build !darwin

package machine

func newDarwin(version string) Machine {
	_ = version
	return NewNative()
}
