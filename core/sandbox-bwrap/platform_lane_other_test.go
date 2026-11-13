//go:build !linux || !integration_bwrap

package bwrap

import "testing"

// The isolation tests in this package need Linux and the integration tag, so on
// any other platform `go test` used to print "[no test files]" — which reads as
// "this backend has no tests" rather than "this backend's tests cannot run here".
// Reporting the reason as a skip makes the coverage visible from the outside.
func TestPlatformLane(t *testing.T) {
	t.Skip("bwrap's isolation tests need linux and -tags=integration_bwrap (make test sets it); " +
		"CI runs them in the Integration core/sandbox-bwrap lane")
}
