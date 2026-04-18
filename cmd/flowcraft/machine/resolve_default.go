//go:build !darwin

package machine

// NewMachine returns a native process manager for Linux (and other UNIX).
func NewMachine(_ string) Machine {
	return NewNative()
}
