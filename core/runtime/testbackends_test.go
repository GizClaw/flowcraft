package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox-remote"
)

// testBackends is the table the internal tests register.
//
// It is written here rather than imported from core/runtime/backends because that
// package imports this one, and an internal test file is compiled as part of the
// package it tests — the import would be a cycle. Writing the two adapters by hand
// is also what a host does, so the test file doubles as the smallest example.
func testBackends() map[string]SandboxFactory {
	return map[string]SandboxFactory{
		BackendLocal: func(_ context.Context, in SandboxBuild) (sandbox.Runner, error) {
			opts := make([]local.Option, 0, 2)
			if in.Limits.MaxOutputBytes > 0 {
				opts = append(opts, local.WithMaxOutputBytes(in.Limits.MaxOutputBytes))
			}
			if in.Journal != nil {
				opts = append(opts, local.WithFileJournalConfig(*in.Journal))
			}
			return local.New(in.Dir, opts...), nil
		},
		BackendRemote: func(ctx context.Context, in SandboxBuild) (sandbox.Runner, error) {
			params, err := in.Config.Remote.BackendParams()
			if err != nil {
				return nil, err
			}
			var opts []remote.Option
			if params.Token != "" {
				opts = append(opts, remote.WithToken(params.Token))
			}
			var httpOpts []remote.HTTPOption
			if params.InsecureSkipVerify {
				httpOpts = append(httpOpts, remote.WithHTTPInsecureTLS())
			}
			if params.Timeout > 0 {
				httpOpts = append(httpOpts, remote.WithHTTPTimeout(params.Timeout))
			}
			if len(httpOpts) > 0 {
				opts = append(opts, remote.WithHTTPOptions(httpOpts...))
			}
			if params.PollHold > 0 {
				opts = append(opts, remote.WithPollHold(params.PollHold))
			}
			if params.Unix {
				return remote.NewUnix(ctx, params.Socket, opts...)
			}
			return remote.New(ctx, params.Endpoint, opts...)
		},
	}
}

// newRuntime is New with that table registered: the tests exercise the sandbox
// policies of a real local runner, so they need one.
func newRuntime(t *testing.T, cfg Config, opts ...Option) (*Runtime, error) {
	t.Helper()
	opts = append(opts, WithSandboxBackends(testBackends()))
	return New(t.Context(), cfg, opts...)
}

// TestUnknownSandboxBackendNamesTheRegisteredSet pins the seam's error path: a
// deployment that names a backend the host did not register must say what this
// process could have built, or an operator has no way to tell a typo from a
// missing registration.
func TestUnknownSandboxBackendNamesTheRegisteredSet(t *testing.T) {
	// bwrap is a backend name the document may carry; it is simply not one this
	// process registered. A name no backend has is rejected by Validate instead,
	// one layer earlier.
	cfg := localConfig(t)
	cfg.Sandbox.Backend = BackendBwrap
	_, err := newRuntime(t, cfg)
	if err == nil {
		t.Fatal("a backend that is not registered was accepted")
	}
	for _, want := range []string{`unknown backend "bwrap"`, "registered: local, remote"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

// TestNoRegisteredBackendSaysNone pins the message a host sees after this change
// if it forgets the table: the one path the tests otherwise never take, because
// newRuntime always registers one.
func TestNoRegisteredBackendSaysNone(t *testing.T) {
	_, err := New(t.Context(), localConfig(t))
	if err == nil {
		t.Fatal("a runtime with no registered backend accepted sandbox.backend: local")
	}
	if !strings.Contains(err.Error(), `unknown backend "local"`) ||
		!strings.Contains(err.Error(), "registered: none") {
		t.Errorf("error = %q, want it to name the backend and report none registered", err)
	}
}

// TestSandboxBackendTableRejectsUnbuildableEntries covers the two shapes a map can
// still get wrong.
func TestSandboxBackendTableRejectsUnbuildableEntries(t *testing.T) {
	local := testBackends()[BackendLocal]
	if _, err := sandboxBackendTable(map[string]SandboxFactory{"": local}); err == nil {
		t.Error("a backend with no name was accepted")
	}
	if _, err := sandboxBackendTable(map[string]SandboxFactory{BackendLocal: nil}); err == nil {
		t.Error("a backend with no constructor was accepted")
	}
}
