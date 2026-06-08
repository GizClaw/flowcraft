package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestAcquireRunPIDWritesAndRemovesOwnPID(t *testing.T) {
	dir := t.TempDir()
	guard, err := acquireRunPID(dir)
	if err != nil {
		t.Fatalf("acquireRunPID: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, runPIDFile))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got, want := string(raw), strconv.Itoa(os.Getpid())+"\n"; got != want {
		t.Fatalf("run.pid = %q, want %q", got, want)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, runPIDFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run.pid should be removed, err=%v", err)
	}
}

func TestAcquireRunPIDTerminatesPreviousPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pid signaling is unix-only")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-waited
		}
	})

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, runPIDFile), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	guard, err := acquireRunPID(dir)
	if err != nil {
		t.Fatalf("acquireRunPID: %v", err)
	}
	defer guard.Close()

	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("previous process did not exit after acquireRunPID")
	}
}

func TestRunPIDGuardDoesNotRemoveAnotherPID(t *testing.T) {
	dir := t.TempDir()
	guard, err := acquireRunPID(dir)
	if err != nil {
		t.Fatalf("acquireRunPID: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, runPIDFile), []byte("99999999\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, runPIDFile))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(raw) != "99999999\n" {
		t.Fatalf("run.pid = %q, want another pid preserved", string(raw))
	}
}
