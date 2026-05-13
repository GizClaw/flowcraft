package sandbox_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// recordingRunner records the most recent Exec call so decorator
// tests can prove that delegated calls reach the inner Runner with
// the expected (post-merge) ExecOptions.
type recordingRunner struct {
	called  bool
	cmd     string
	gotOpts sandbox.ExecOptions
}

func (r *recordingRunner) Exec(_ context.Context, cmd string, _ []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	r.called = true
	r.cmd = cmd
	r.gotOpts = opts
	return &sandbox.ExecResult{Stdout: "ok"}, nil
}

func TestAllowCommands_Pass(t *testing.T) {
	inner := &recordingRunner{}
	r := sandbox.AllowCommands(inner, []string{"ls", "cat", "echo"})

	result, err := r.Exec(context.Background(), "ls", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("allowed command should succeed: %v", err)
	}
	if !inner.called {
		t.Fatal("allowed command should reach inner runner")
	}
	if inner.cmd != "ls" {
		t.Fatalf("inner.cmd = %q, want 'ls'", inner.cmd)
	}
	if result.Stdout != "ok" {
		t.Fatalf("result not passed through, got Stdout = %q", result.Stdout)
	}
}

func TestAllowCommands_Reject(t *testing.T) {
	inner := &recordingRunner{}
	r := sandbox.AllowCommands(inner, []string{"ls", "cat"})

	_, err := r.Exec(context.Background(), "rm", []string{"-rf", "/"}, sandbox.ExecOptions{})
	if err == nil {
		t.Fatal("blocked command should fail")
	}
	if !strings.Contains(err.Error(), "whitelist") {
		t.Fatalf("error should mention whitelist, got: %v", err)
	}
	if inner.called {
		t.Fatal("blocked command must not reach inner runner")
	}
}

func TestAllowCommands_EmptyWhitelist(t *testing.T) {
	inner := &recordingRunner{}
	r := sandbox.AllowCommands(inner, nil)

	_, err := r.Exec(context.Background(), "ls", nil, sandbox.ExecOptions{})
	if err == nil {
		t.Fatal("empty whitelist should block all commands")
	}
	if inner.called {
		t.Fatal("empty whitelist must not reach inner runner")
	}
}

// -- WithDefaults --

func TestWithDefaults_WorkDir_CallerWins(t *testing.T) {
	inner := &recordingRunner{}
	r := sandbox.WithDefaults(inner, sandbox.ExecOptions{WorkDir: "from-defaults"})

	_, err := r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{WorkDir: "from-caller"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if inner.gotOpts.WorkDir != "from-caller" {
		t.Fatalf("WorkDir = %q, want 'from-caller' (caller should win)", inner.gotOpts.WorkDir)
	}
}

func TestWithDefaults_WorkDir_FallsBackToDefault(t *testing.T) {
	inner := &recordingRunner{}
	r := sandbox.WithDefaults(inner, sandbox.ExecOptions{WorkDir: "from-defaults"})

	_, err := r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if inner.gotOpts.WorkDir != "from-defaults" {
		t.Fatalf("WorkDir = %q, want 'from-defaults' (empty caller should fall back)", inner.gotOpts.WorkDir)
	}
}

func TestWithDefaults_Stdin_CallerWinsAndFallback(t *testing.T) {
	t.Run("caller wins", func(t *testing.T) {
		inner := &recordingRunner{}
		r := sandbox.WithDefaults(inner, sandbox.ExecOptions{Stdin: []byte("default-stdin")})

		_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{Stdin: []byte("caller-stdin")})
		if string(inner.gotOpts.Stdin) != "caller-stdin" {
			t.Fatalf("Stdin = %q, want 'caller-stdin'", inner.gotOpts.Stdin)
		}
	})
	t.Run("nil falls back", func(t *testing.T) {
		inner := &recordingRunner{}
		r := sandbox.WithDefaults(inner, sandbox.ExecOptions{Stdin: []byte("default-stdin")})

		_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{})
		if string(inner.gotOpts.Stdin) != "default-stdin" {
			t.Fatalf("Stdin = %q, want 'default-stdin'", inner.gotOpts.Stdin)
		}
	})
}

func TestWithDefaults_Timeout_MinRule(t *testing.T) {
	// The four corners of (caller, default) × (zero, positive) plus
	// the actual narrowing case. Locked down because timeout is the
	// one place defaults act as a ceiling — any future refactor that
	// flips min() to max() would silently let tools escape the
	// sandbox-imposed window.
	tests := []struct {
		name       string
		callerTO   time.Duration
		defaultTO  time.Duration
		wantMerged time.Duration
	}{
		{"both zero", 0, 0, 0},
		{"caller zero, default non-zero", 0, 5 * time.Second, 5 * time.Second},
		{"caller non-zero, default zero", 3 * time.Second, 0, 3 * time.Second},
		{"caller narrows", 2 * time.Second, 5 * time.Second, 2 * time.Second},
		{"caller would broaden — default wins (ceiling)", 10 * time.Second, 4 * time.Second, 4 * time.Second},
		{"equal", 5 * time.Second, 5 * time.Second, 5 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner := &recordingRunner{}
			r := sandbox.WithDefaults(inner, sandbox.ExecOptions{Timeout: tc.defaultTO})

			_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{Timeout: tc.callerTO})
			if inner.gotOpts.Timeout != tc.wantMerged {
				t.Fatalf("Timeout = %v, want %v", inner.gotOpts.Timeout, tc.wantMerged)
			}
		})
	}
}

func TestWithDefaults_Env_AllowDefaultsAlwaysWins(t *testing.T) {
	// Allow-list narrowing at call time is not actionable for this
	// MVP (defaults owns the list); the contract is that the inner
	// Runner sees defaults.Allow regardless of what the caller
	// supplied. This protects against a buggy / malicious tool
	// trying to widen the host-env view by passing
	// Env.Allow=[..., "SECRET_KEY"].
	defaults := sandbox.EnvPolicy{Allow: []string{"PATH", "HOME"}}

	tests := []struct {
		name        string
		callerAllow []string
		want        []string
	}{
		{"nil caller", nil, []string{"PATH", "HOME"}},
		{"empty caller", []string{}, []string{"PATH", "HOME"}},
		{"caller tries to widen", []string{"PATH", "HOME", "SECRET_KEY"}, []string{"PATH", "HOME"}},
		{"caller tries to narrow", []string{"PATH"}, []string{"PATH", "HOME"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner := &recordingRunner{}
			r := sandbox.WithDefaults(inner, sandbox.ExecOptions{Env: defaults})

			_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{
				Env: sandbox.EnvPolicy{Allow: tc.callerAllow},
			})
			if !reflect.DeepEqual(inner.gotOpts.Env.Allow, tc.want) {
				t.Fatalf("Env.Allow = %v, want %v", inner.gotOpts.Env.Allow, tc.want)
			}
		})
	}
}

func TestWithDefaults_Env_InjectUnion_CallerWinsOnConflict(t *testing.T) {
	defaults := sandbox.EnvPolicy{
		Inject: map[string]string{
			"SANDBOX_ID": "default-id",
			"REGION":     "us-east",
		},
	}
	inner := &recordingRunner{}
	r := sandbox.WithDefaults(inner, sandbox.ExecOptions{Env: defaults})

	_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{
			Inject: map[string]string{
				"SANDBOX_ID": "caller-id", // collision — caller wins
				"RUN_ID":     "abc123",    // new key — appended
			},
		},
	})

	got := inner.gotOpts.Env.Inject
	want := map[string]string{
		"SANDBOX_ID": "caller-id",
		"REGION":     "us-east",
		"RUN_ID":     "abc123",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Env.Inject = %v, want %v", got, want)
	}
}

func TestWithDefaults_Env_InjectBothEmpty_StaysEmpty(t *testing.T) {
	// Guard against a regression where we always allocate a map
	// for the merged Inject (would surface as Env.Inject == empty
	// non-nil map vs. nil — the LocalRunner treats both the same,
	// but downstream observers / serialisers may not).
	inner := &recordingRunner{}
	r := sandbox.WithDefaults(inner, sandbox.ExecOptions{})

	_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{})
	if inner.gotOpts.Env.Inject != nil {
		t.Fatalf("Env.Inject = %v, want nil when both sides are empty", inner.gotOpts.Env.Inject)
	}
}

func TestWithDefaults_Net_DefaultsOnly(t *testing.T) {
	// NetPolicy is sandbox-level policy: the caller cannot weaken
	// (e.g. switch DenyAll → Default) or strengthen (Default →
	// DenyAll only to escape an AllowList). Tools that want a
	// different posture run against a different Sandbox resource.
	defaults := sandbox.NetPolicy{Mode: sandbox.NetDenyAll}
	inner := &recordingRunner{}
	r := sandbox.WithDefaults(inner, sandbox.ExecOptions{Net: defaults})

	_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{
		Net: sandbox.NetPolicy{Mode: sandbox.NetDefault, AllowHosts: []string{"any.host"}},
	})
	if inner.gotOpts.Net.Mode != sandbox.NetDenyAll {
		t.Fatalf("Net.Mode = %v, want NetDenyAll (defaults must win)", inner.gotOpts.Net.Mode)
	}
	if len(inner.gotOpts.Net.AllowHosts) != 0 {
		t.Fatalf("Net.AllowHosts = %v, want empty (caller AllowHosts should be ignored)", inner.gotOpts.Net.AllowHosts)
	}
}

func TestWithDefaults_Resources_DefaultsOnly(t *testing.T) {
	defaults := sandbox.ResourceLimits{
		CPUMillicores:  500,
		MemoryBytes:    256 << 20,
		DiskBytes:      1 << 30,
		MaxOutputBytes: 1 << 20,
	}
	inner := &recordingRunner{}
	r := sandbox.WithDefaults(inner, sandbox.ExecOptions{Resources: defaults})

	_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{
		Resources: sandbox.ResourceLimits{
			CPUMillicores:  8000,      // tool would never get to raise CPU
			MemoryBytes:    16 << 30,  // ... or memory
			MaxOutputBytes: 100 << 20, // ... or output cap
		},
	})
	if !reflect.DeepEqual(inner.gotOpts.Resources, defaults) {
		t.Fatalf("Resources = %+v, want defaults %+v (caller must not raise caps)", inner.gotOpts.Resources, defaults)
	}
}

func TestWithDefaults_DefaultsNotMutated(t *testing.T) {
	// Multiple Exec calls must not see each other through shared
	// state. The merge path allocates a fresh map for Env.Inject
	// rather than mutating defaults.Inject — this test pins that
	// guarantee.
	defaults := sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{
			Inject: map[string]string{"REGION": "us-east"},
		},
	}
	inner := &recordingRunner{}
	r := sandbox.WithDefaults(inner, defaults)

	_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{Inject: map[string]string{"RUN_ID": "first"}},
	})
	_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{Inject: map[string]string{"RUN_ID": "second"}},
	})

	// defaults.Env.Inject must still be exactly what we passed in.
	if !reflect.DeepEqual(defaults.Env.Inject, map[string]string{"REGION": "us-east"}) {
		t.Fatalf("defaults.Env.Inject mutated: got %v", defaults.Env.Inject)
	}
	// The second call's inner.gotOpts must NOT carry the first
	// call's RUN_ID — proves the merged map is per-call.
	if inner.gotOpts.Env.Inject["RUN_ID"] != "second" {
		t.Fatalf("second call RUN_ID = %q, want 'second' (cross-call leak suspected)", inner.gotOpts.Env.Inject["RUN_ID"])
	}
}

func TestWithDefaults_EmptyCallerOpts(t *testing.T) {
	// The "all-defaults" path: caller passes a bare ExecOptions{}.
	// Every field on the merged result must come from defaults.
	// Guards the "this is what a Tool that knows nothing about
	// sandbox policy will pass" call shape.
	defaults := sandbox.ExecOptions{
		WorkDir: "/work",
		Stdin:   []byte("default-stdin"),
		Timeout: 7 * time.Second,
		Env: sandbox.EnvPolicy{
			Allow:  []string{"PATH"},
			Inject: map[string]string{"REGION": "us-east"},
		},
		Net:       sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
		Resources: sandbox.ResourceLimits{MaxOutputBytes: 1 << 20},
	}
	inner := &recordingRunner{}
	r := sandbox.WithDefaults(inner, defaults)

	_, _ = r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{})

	if inner.gotOpts.WorkDir != defaults.WorkDir {
		t.Errorf("WorkDir = %q, want %q", inner.gotOpts.WorkDir, defaults.WorkDir)
	}
	if string(inner.gotOpts.Stdin) != string(defaults.Stdin) {
		t.Errorf("Stdin = %q, want %q", inner.gotOpts.Stdin, defaults.Stdin)
	}
	if inner.gotOpts.Timeout != defaults.Timeout {
		t.Errorf("Timeout = %v, want %v", inner.gotOpts.Timeout, defaults.Timeout)
	}
	if !reflect.DeepEqual(inner.gotOpts.Env.Allow, defaults.Env.Allow) {
		t.Errorf("Env.Allow = %v, want %v", inner.gotOpts.Env.Allow, defaults.Env.Allow)
	}
	if !reflect.DeepEqual(inner.gotOpts.Env.Inject, defaults.Env.Inject) {
		t.Errorf("Env.Inject = %v, want %v", inner.gotOpts.Env.Inject, defaults.Env.Inject)
	}
	if inner.gotOpts.Net.Mode != defaults.Net.Mode {
		t.Errorf("Net.Mode = %v, want %v", inner.gotOpts.Net.Mode, defaults.Net.Mode)
	}
	if inner.gotOpts.Resources != defaults.Resources {
		t.Errorf("Resources = %+v, want %+v", inner.gotOpts.Resources, defaults.Resources)
	}
}

func TestWithDefaults_ComposesWithAllowCommands(t *testing.T) {
	// The documented inner-to-outer composition: LocalRunner →
	// AllowCommands → WithDefaults. The whitelist gate fires before
	// the inner Runner is reached, regardless of how rich the
	// defaults are — so a blocked command never gets its
	// ExecOptions merged at all.
	inner := &recordingRunner{}
	gated := sandbox.AllowCommands(inner, []string{"echo"})
	r := sandbox.WithDefaults(gated, sandbox.ExecOptions{Timeout: 5 * time.Second})

	if _, err := r.Exec(context.Background(), "rm", nil, sandbox.ExecOptions{}); err == nil {
		t.Fatal("blocked command should still be rejected through the WithDefaults wrapper")
	}
	if inner.called {
		t.Fatal("blocked command must not reach the inner runner")
	}

	if _, err := r.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{}); err != nil {
		t.Fatalf("allowed command should pass: %v", err)
	}
	if inner.gotOpts.Timeout != 5*time.Second {
		t.Fatalf("Timeout = %v, want 5s (defaults should have been merged)", inner.gotOpts.Timeout)
	}
}

func TestNoopRunner(t *testing.T) {
	result, err := sandbox.NoopRunner{}.Exec(context.Background(), "anything", []string{"arg"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("NoopRunner should not error: %v", err)
	}
	if result == nil {
		t.Fatal("NoopRunner should return non-nil ExecResult")
	}
	if result.ExitCode != 0 || result.Stdout != "" || result.Stderr != "" {
		t.Fatalf("NoopRunner should return empty result, got: %+v", *result)
	}
}
