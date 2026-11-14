//go:build !windows

package windows

import "testing"

// The Windows backend's tests need a Windows host, so elsewhere `go test` used to
// print "[no test files]" — which reads as "no tests" rather than "not here".
func TestPlatformLane(t *testing.T) {
	t.Skip("the windows backend's tests need a windows host; " +
		"CI runs them in the Integration core/sandbox-windows lane")
}
