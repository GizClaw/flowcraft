//go:build unix

package local_test

// localSessionsSupported mirrors the package's own session files: the
// local backend's process sessions exist on unix only
// (core/sandbox/local/session_other.go). A test that needs the backend
// to drive a real command — the journal contract suite — has nothing to
// drive elsewhere, so it skips rather than failing on the platform.
const localSessionsSupported = true
