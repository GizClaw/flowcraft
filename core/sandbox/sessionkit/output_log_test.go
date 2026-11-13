//go:build unix

package sessionkit

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// The configured budget replays a live session. Once it exits, keeping the whole
// capture pins megabytes for a process nobody will read from again.
func TestFinishedLogRetainsOnlyATail(t *testing.T) {
	log := newOutputLog(8 << 20)
	chunk := make([]byte, 256<<10)
	for i := 0; i < 8; i++ { // 2 MiB captured
		log.append(sandbox.SessionStreamStdout, chunk)
	}
	if log.total <= exitedRetainBytes {
		t.Fatalf("append retained %d bytes, the test never exercised the trim", log.total)
	}
	log.finish(sandbox.SessionExit{})
	if log.total > exitedRetainBytes {
		t.Errorf("after exit the log retains %d bytes, want at most %d",
			log.total, exitedRetainBytes)
	}
	log.close()
}
