//go:build !windows

package windows

// MaybeHelper is the non-Windows stub: the elevated backend is
// unavailable on this platform, so no re-executed helper invocation
// can occur and the hook always reports "not the helper". It exists
// so portable host applications can call windows.MaybeHelper()
// unconditionally at the top of main without build-tag gymnastics.
func MaybeHelper() bool {
	return false
}
