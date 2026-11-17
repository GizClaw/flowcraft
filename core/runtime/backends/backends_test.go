package backends

import (
	"context"
	"strings"
	"testing"

	goruntime "runtime"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
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

// TestEveryFactoryRuns executes each adapter at least once.
//
// The consumers in this repository all assemble documents that say backend: local,
// so without this the bwrap, seatbelt and windows adapters are only ever
// constructed — a wrong field name or a nil-configuration dereference would reach
// a deployment before a test. Local is asserted to build; the platform-dependent
// ones are asserted to build or to report NotAvailable, which is what shipping a
// backend on the wrong OS looks like, and their own option blocks are set so the
// branches that read them run where the platform allows.
func TestEveryFactoryRuns(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writes := t.TempDir() // not the root: a read-only root refuses that combination

	t.Run("local", func(t *testing.T) {
		runner, err := Local()(ctx, runtime.SandboxBuild{
			Dir:     dir,
			Limits:  sandbox.ResourceLimits{MaxOutputBytes: 4096},
			Journal: &journal.Config{Root: dir},
		})
		if err != nil {
			t.Fatalf("Local(): %v", err)
		}
		if runner == nil {
			t.Fatal("Local() returned no runner")
		}
		// New stores a journal failure rather than returning it, so the build
		// having read the block is only visible here.
		if journal.Available() && !runner.Capabilities().Journal.Enabled {
			t.Error("the build asked for a journal and the runner attached none")
		}
		if err := runner.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	// The two bwrap cases are only meaningful where the constructor is real: on
	// another OS the package is a stub that refuses before reading the options, so
	// accepting NotAvailable there would assert nothing about the adapter.
	t.Run("bwrap rejects a policy flag", func(t *testing.T) {
		requireBwrap(t)
		_, err := Bwrap()(ctx, runtime.SandboxBuild{
			Dir: dir,
			Config: runtime.SandboxConfig{Bwrap: &runtime.BwrapConfig{
				ExtraFlags: []string{"--bind"},
			}},
		})
		if err == nil {
			t.Fatal("a flag that weakens enforcement was accepted")
		}
		// validateExtraFlags runs before the binary lookup, so this holds whether
		// or not bubblewrap is installed — and fails if ExtraFlags stops reaching
		// the constructor.
		if !strings.Contains(err.Error(), "weaken sandbox enforcement") {
			t.Errorf("error = %q, want the policy refusal", err)
		}
	})

	t.Run("bwrap reports a binary it cannot run", func(t *testing.T) {
		requireBwrap(t)
		_, err := Bwrap()(ctx, runtime.SandboxBuild{
			Dir: dir,
			Config: runtime.SandboxConfig{Bwrap: &runtime.BwrapConfig{
				Binary: "/nonexistent/flowcraft-bwrap",
			}},
		})
		if err == nil {
			t.Fatal("a binary that does not exist was accepted")
		}
		if !errdefs.IsNotAvailable(err) {
			t.Fatalf("error = %v, want NotAvailable", err)
		}
		// Naming the configured path is what separates "the adapter forwarded
		// Binary" from "the adapter dropped it and the PATH lookup failed": both
		// are NotAvailable, and only one of them is the bug this test is for.
		if !strings.Contains(err.Error(), "/nonexistent/flowcraft-bwrap") {
			t.Errorf("error = %q, want it to name the configured binary", err)
		}
	})

	t.Run("seatbelt reports a binary it cannot run", func(t *testing.T) {
		if goruntime.GOOS != "darwin" {
			t.Skipf("seatbelt is a compile-time stub on %s", goruntime.GOOS)
		}
		_, err := Seatbelt()(ctx, runtime.SandboxBuild{
			Dir: dir,
			Config: runtime.SandboxConfig{Seatbelt: &runtime.SeatbeltConfig{
				Binary: "/nonexistent/sandbox-exec",
			}},
		})
		if err == nil {
			t.Fatal("a binary that does not exist was accepted")
		}
		if !errdefs.IsNotAvailable(err) || !strings.Contains(err.Error(), "/nonexistent/sandbox-exec") {
			t.Errorf("error = %v, want NotAvailable naming the configured binary", err)
		}
	})

	// Seatbelt and windows build a runner where the platform implements them and
	// report NotAvailable elsewhere. Either outcome executes the adapter, which is
	// the point; a panic or a nil runner with no error is not.
	cases := map[string]struct {
		build  runtime.SandboxFactory
		config runtime.SandboxConfig
	}{
		"seatbelt": {
			build:  Seatbelt(),
			config: runtime.SandboxConfig{Seatbelt: &runtime.SeatbeltConfig{ReadOnlyRoot: true}},
		},
		"windows": {
			build:  Windows(),
			config: runtime.SandboxConfig{Windows: &runtime.WindowsConfig{WriteConfine: true}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runner, err := tc.build(ctx, runtime.SandboxBuild{
				Dir:    dir,
				Writes: []string{writes},
				Config: tc.config,
			})
			if err != nil {
				if !errdefs.IsNotAvailable(err) {
					t.Fatalf("%s() = %v, want a runner or NotAvailable", name, err)
				}
				return
			}
			if runner == nil {
				t.Fatalf("%s() returned neither a runner nor an error", name)
			}
			if err := runner.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		})
	}
}

// requireBwrap skips where the bwrap package is a stub: there the constructor
// refuses before it reads a single option, so an assertion about options would be
// vacuously true.
func requireBwrap(t *testing.T) {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skipf("bwrap is a compile-time stub on %s", goruntime.GOOS)
	}
}
