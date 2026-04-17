package sandbox

import (
	"context"
	"strings"
	"testing"
)

func assertContains(t *testing.T, args []string, target string) {
	t.Helper()
	for _, a := range args {
		if a == target {
			return
		}
	}
	t.Fatalf("expected args to contain %q, got %v", target, args)
}

func assertContainsSequence(t *testing.T, args []string, seq ...string) {
	t.Helper()
	for i := 0; i <= len(args)-len(seq); i++ {
		match := true
		for j, s := range seq {
			if args[i+j] != s {
				match = false
				break
			}
		}
		if match {
			return
		}
	}
	t.Fatalf("expected args to contain sequence %v, got %v", seq, args)
}

func indexOf(args []string, target string) int {
	for i, a := range args {
		if a == target {
			return i
		}
	}
	return -1
}

func TestBuildBwrapCommand_BasicArgs(t *testing.T) {
	cmd := buildBwrapCommand(
		context.Background(),
		"/usr/bin/bwrap",
		"/tmp/sandbox/sess1",
		"/tmp/sandbox/sess1",
		[]string{"/data/skills"},
		[]string{"/data/user-output"},
		"python3", []string{"main.py"},
		minimalEnv("/tmp/sandbox/sess1", nil),
		BwrapConfig{},
	)

	args := cmd.Args
	assertContains(t, args, "--unshare-pid")
	assertContains(t, args, "--unshare-net")
	assertContains(t, args, "--unshare-user")
	assertContains(t, args, "--die-with-parent")
	assertContains(t, args, "--clearenv")
	assertContainsSequence(t, args, "--chdir", "/tmp/sandbox/sess1")
	assertContainsSequence(t, args, "--ro-bind-try", "/sbin", "/sbin")

	assertContainsSequence(t, args, "--ro-bind-try", "/data/skills", "/data/skills")
	assertContainsSequence(t, args, "--bind-try", "/data/user-output", "/data/user-output")

	sepIdx := indexOf(args, "--")
	if sepIdx < 0 {
		t.Fatal("missing -- separator")
	}
	if args[sepIdx+1] != "python3" || args[sepIdx+2] != "main.py" {
		t.Fatalf("unexpected command after separator: %v", args[sepIdx+1:])
	}
}

func TestBuildBwrapCommand_ShareNet(t *testing.T) {
	cmd := buildBwrapCommand(
		context.Background(),
		"/usr/bin/bwrap",
		"/tmp/sandbox/sess1",
		"/tmp/sandbox/sess1",
		nil, nil,
		"curl", []string{"https://example.com"},
		minimalEnv("/tmp/sandbox/sess1", nil),
		BwrapConfig{ShareNet: true},
	)

	args := cmd.Args
	for _, arg := range args {
		if arg == "--unshare-net" {
			t.Fatal("--unshare-net should not be present when ShareNet=true")
		}
	}
	assertContains(t, args, "/etc/resolv.conf")
}

func TestBuildBwrapCommand_NoTargets(t *testing.T) {
	cmd := buildBwrapCommand(
		context.Background(),
		"/usr/bin/bwrap",
		"/tmp/sandbox/sess1",
		"/tmp/sandbox/sess1",
		nil, nil,
		"echo", []string{"hello"},
		minimalEnv("/tmp/sandbox/sess1", nil),
		BwrapConfig{},
	)

	args := cmd.Args
	for i, a := range args {
		if a == "--bind-try" {
			t.Fatalf("unexpected --bind-try at index %d with nil targets", i)
		}
	}
}

func TestMinimalEnv_NoHostLeakage(t *testing.T) {
	env := minimalEnv("/workspace", map[string]string{"MY_KEY": "val"})

	hasPath, hasTerm, hasCustom := false, false, false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
		}
		if e == "TERM=dumb" {
			hasTerm = true
		}
		if e == "MY_KEY=val" {
			hasCustom = true
		}
		if strings.HasPrefix(e, "AWS_") || strings.HasPrefix(e, "OPENAI_") {
			t.Fatalf("minimalEnv should not contain host secrets: %s", e)
		}
	}
	if !hasPath {
		t.Fatal("minimalEnv must include PATH")
	}
	if !hasTerm {
		t.Fatal("minimalEnv must include TERM=dumb")
	}
	if !hasCustom {
		t.Fatal("minimalEnv must include custom extras")
	}
}

func TestMinimalEnv_Home(t *testing.T) {
	env := minimalEnv("/my/root", nil)
	for _, e := range env {
		if e == "HOME=/my/root" {
			return
		}
	}
	t.Fatal("minimalEnv must set HOME to rootDir")
}
