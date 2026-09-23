//go:build !unix

package local_test

// localSessionsSupported mirrors core/sandbox/local/session_other.go:
// without unix process sessions there is no command to run, and no
// sandboxed write for the journal contract to observe.
const localSessionsSupported = false
