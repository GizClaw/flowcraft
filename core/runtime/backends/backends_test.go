package backends

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/runtime"
)

// TestAllCoversEveryDocumentedBackend pins the table against the names a
// deployment may write. It is also the only place the factories are called at all:
// the consumers in this repository assemble runtimes over documents that say
// backend: local, so a wrong key or a dropped adapter in the other four would
// otherwise pass the whole suite.
func TestAllCoversEveryDocumentedBackend(t *testing.T) {
	all := All()
	want := []string{
		runtime.BackendLocal,
		runtime.BackendBwrap,
		runtime.BackendSeatbelt,
		runtime.BackendWindows,
		runtime.BackendRemote,
	}
	if len(all) != len(want) {
		t.Fatalf("All() has %d entries, want %d", len(all), len(want))
	}
	for _, name := range want {
		build, ok := all[name]
		if !ok {
			t.Errorf("All() is missing %q, which a document may name", name)
			continue
		}
		if build == nil {
			t.Errorf("%q has no constructor", name)
		}
	}
}

// TestRemoteWrapsASocketFailure pins the one adapter path that is measurable
// without the OS features the others need: a unix socket that does not exist must
// fail as sandbox.remote.socket, the shape the runtime has always reported.
func TestRemoteWrapsASocketFailure(t *testing.T) {
	build := Remote()
	_, err := build(context.Background(), runtime.SandboxBuild{
		Config: runtime.SandboxConfig{
			Backend: runtime.BackendRemote,
			Remote: runtime.RemoteConfig{
				Transport: "unix",
				Socket:    "/nonexistent/flowcraft/sandbox.sock",
			},
		},
	})
	if err == nil {
		t.Fatal("a socket that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "sandbox.remote.socket") {
		t.Errorf("error = %q, want it to name sandbox.remote.socket", err)
	}
}
