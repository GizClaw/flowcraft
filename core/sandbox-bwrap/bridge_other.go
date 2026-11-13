//go:build !linux

package bwrap

// MaybeBridge is the non-Linux stub: the bwrap backend is unavailable
// on this platform, so no re-executed bridge invocation can occur and
// the hook always reports "not the bridge". It exists so portable host
// applications can call bwrap.MaybeBridge() unconditionally at the top
// of main without build-tag gymnastics.
func MaybeBridge() bool {
	return false
}
