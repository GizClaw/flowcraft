package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const runPIDFile = "run.pid"

type runPIDGuard struct {
	path string
	pid  int
}

func acquireRunPID(workspaceDir string) (*runPIDGuard, error) {
	root := strings.TrimSpace(workspaceDir)
	if root == "" {
		root = "workspace"
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace root: %w", err)
	}

	path := filepath.Join(root, runPIDFile)
	if oldPID, err := readRunPID(path); err == nil && oldPID > 0 && oldPID != os.Getpid() {
		if err := terminatePID(oldPID, 10*time.Second); err != nil {
			return nil, fmt.Errorf("stop previous run pid %d: %w", oldPID, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(path)
	}

	pid := os.Getpid()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("write run pid: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("install run pid: %w", err)
	}
	return &runPIDGuard{path: path, pid: pid}, nil
}

func (g *runPIDGuard) Close() error {
	if g == nil || g.path == "" {
		return nil
	}
	pid, err := readRunPID(g.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if pid != g.pid {
		return nil
	}
	if err := os.Remove(g.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readRunPID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return 0, fmt.Errorf("empty pid file")
	}
	pid, err := strconv.Atoi(text)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pid file %q", text)
	}
	return pid, nil
}

func terminatePID(pid int, timeout time.Duration) error {
	alive, err := processAlive(pid)
	if err != nil {
		return err
	}
	if !alive {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, err := processAlive(pid)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("pid %d did not exit within %s", pid, timeout)
}

func processAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	return false, err
}
