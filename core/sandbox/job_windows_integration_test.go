//go:build windows && integration_windows

package sandbox

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestIntegrationJobCPUSampling verifies the two primitives the
// cpu-time watcher depends on — job membership enumeration and
// GetProcessTimes aggregation — against a real process inside a real
// job. When a cpu-cap integration test fails, this test separates
// "sampling is broken" from "the child never joined the job".
func TestIntegrationJobCPUSampling(t *testing.T) {
	job, err := newJobObject()
	if err != nil {
		t.Fatalf("newJobObject: %v", err)
	}
	defer func() { _ = job.Close() }()

	cmd := exec.Command("cmd", "/c", "ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		t.Fatalf("OpenProcess(%d): %v", cmd.Process.Pid, err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()

	if err := windows.AssignProcessToJobObject(job.handle, proc); err != nil {
		t.Fatalf("AssignProcessToJobObject: %v", err)
	}

	// Give the child a moment to actually consume cpu time before the
	// first sample.
	time.Sleep(1500 * time.Millisecond)

	pids, err := job.processIDs()
	if err != nil {
		t.Fatalf("processIDs: %v", err)
	}
	found := false
	for _, pid := range pids {
		if pid == uint32(cmd.Process.Pid) {
			found = true
		}
	}
	if !found {
		t.Fatalf("job process list %v does not contain child pid %d", pids, cmd.Process.Pid)
	}
	// On a loaded CI runner the child may not have been scheduled yet;
	// poll until it has accumulated cpu time instead of sampling once.
	deadline := time.Now().Add(8 * time.Second)
	for {
		cpu, err := job.sampleCPU()
		if err != nil {
			t.Fatalf("sampleCPU: %v", err)
		}
		if cpu > 0 {
			return
		}
		if time.Now().After(deadline) {
			var detail strings.Builder
			for _, pid := range pids {
				h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
				if err != nil {
					fmt.Fprintf(&detail, "pid %d: OpenProcess: %v; ", pid, err)
					continue
				}
				var ct, et, kt, ut windows.Filetime
				err = windows.GetProcessTimes(h, &ct, &et, &kt, &ut)
				_ = windows.CloseHandle(h)
				if err != nil {
					fmt.Fprintf(&detail, "pid %d: GetProcessTimes: %v; ", pid, err)
					continue
				}
				fmt.Fprintf(&detail, "pid %d: kt=%d ut=%d; ", pid, uint64(kt.HighDateTime)<<32|uint64(kt.LowDateTime), uint64(ut.HighDateTime)<<32|uint64(ut.LowDateTime))
			}
			t.Fatalf("sampleCPU stayed 0s for a running child (pids %v): %s", pids, detail.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
}
