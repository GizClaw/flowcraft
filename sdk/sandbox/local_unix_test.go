//go:build unix

package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// TestHelperProcess is re-executed by the resource-limit tests as the
// child command. It is inert unless FLOWCRAFT_TEST_HELPER=1 is injected.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("FLOWCRAFT_TEST_HELPER") != "1" {
		return
	}
	switch os.Getenv("FLOWCRAFT_TEST_HELPER_MODE") {
	case "alloc":
		fmt.Println("helper started")
		chunks := make([][]byte, 0, 512)
		for i := 0; i < 512; i++ {
			b := make([]byte, 1<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1 // fault every page in so it counts against RSS
			}
			chunks = append(chunks, b)
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Println("helper allocated 512MiB")
		// Sleep, not select{}: a bare select trips Go's deadlock
		// detector and exits on its own, which fakes the kill signal
		// this helper exists to verify.
		time.Sleep(time.Hour)
	case "spin":
		fmt.Println("helper started")
		for {
		}
	}
	os.Exit(0)
}

func helperExec(mode string) (string, []string, sandbox.EnvPolicy) {
	env := sandbox.EnvPolicy{
		Allow: []string{"PATH", "HOME"},
		Inject: map[string]string{
			"FLOWCRAFT_TEST_HELPER":      "1",
			"FLOWCRAFT_TEST_HELPER_MODE": mode,
		},
	}
	return os.Args[0], []string{"-test.run", "^TestHelperProcess$"}, env
}

func TestLocalRunner_MemoryLimit_KillsChild(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	cmd, args, env := helperExec("alloc")

	start := time.Now()
	result, err := runner.Exec(context.Background(), cmd, args, sandbox.ExecOptions{
		Env:       env,
		Timeout:   30 * time.Second,
		Resources: sandbox.ResourceLimits{MemoryBytes: 128 << 20},
	})
	elapsed := time.Since(start)
	if !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("memory-capped child should return BudgetExceeded, got: %v", err)
	}
	if !strings.Contains(result.Stdout, "helper started") {
		t.Fatalf("child should have started before being killed, stdout=%q", result.Stdout)
	}
	if strings.Contains(result.Stdout, "allocated 512MiB") {
		t.Fatal("child reached 512MiB despite the 128MiB group RSS cap")
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("child ran %v; watcher should have killed it shortly after the first sample", elapsed)
	}
}

func TestLocalRunner_CPUTimeLimit_KillsChild(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	cmd, args, env := helperExec("spin")

	start := time.Now()
	_, err := runner.Exec(context.Background(), cmd, args, sandbox.ExecOptions{
		Env:     env,
		Timeout: 4 * time.Second,
		// cpu-time cap = 4s x 250/1000 = 1s, far below the wall timeout.
		Resources: sandbox.ResourceLimits{CPUMillicores: 250},
	})
	elapsed := time.Since(start)
	if !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("cpu-capped child should return BudgetExceeded, got: %v", err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("child ran %v, expected to die near the 1s cpu cap", elapsed)
	}
}

func TestLocalRunner_ProcessGroupKilledOnTimeout(t *testing.T) {
	root := t.TempDir()
	runner := sandbox.NewLocalRunner(root)

	pidfile := filepath.Join(root, "child.pid")
	// Spawn a background descendant, record its pid, then block in wait.
	// On timeout the runner must kill the whole group — descendant included.
	script := "sleep 60 & echo $! > " + pidfile + "; wait"
	_, err := runner.Exec(context.Background(), "sh", []string{"-c", script}, sandbox.ExecOptions{
		Timeout: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errdefs.IsTimeout(err) {
		t.Fatalf("expected errdefs.IsTimeout, got: %v", err)
	}

	time.Sleep(150 * time.Millisecond) // let SIGKILL land
	data, rerr := os.ReadFile(pidfile)
	if rerr != nil {
		t.Fatalf("read pidfile: %v", rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil {
		t.Fatalf("parse pid: %v", perr)
	}
	if kerr := syscall.Kill(pid, 0); kerr == nil {
		t.Fatalf("background descendant pid %d survived the process-group kill", pid)
	} else if kerr != syscall.ESRCH {
		t.Logf("signal-0 on pid %d returned %v (acceptable)", pid, kerr)
	}
}

// Sanity: without limits the shell wrapper must not engage, so exec
// semantics stay byte-identical to the pre-enforcement behaviour.
func TestLocalRunner_NoLimits_DoesNotInvokeShell(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	result, err := runner.Exec(context.Background(), "sh", []string{"-c", "printf %s \"$0\""}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "sh" {
		t.Fatalf("unexpected wrapper: $0 = %q, want 'sh'", result.Stdout)
	}
}

// The rlimit path must preserve the caller-visible argv and exit code
// through "exec \"$@\"".
func TestLocalRunner_WithLimits_PreservesArgAndExitSemantics(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	result, err := runner.Exec(context.Background(), "sh", []string{"-c", "printf %s \"$1\"; exit 7", "sh", "payload"}, sandbox.ExecOptions{
		Timeout:   5 * time.Second,
		Resources: sandbox.ResourceLimits{MemoryBytes: 256 << 20, CPUMillicores: 1000},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}
	if result.Stdout != "payload" {
		t.Fatalf("Stdout = %q, want 'payload'", result.Stdout)
	}
}
