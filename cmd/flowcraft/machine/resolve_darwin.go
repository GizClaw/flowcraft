//go:build darwin

package machine

func newDarwin(version string) Machine {
	return NewLimaVM(version)
}
