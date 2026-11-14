//go:build windows

package sessionkit

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
	xwin "golang.org/x/sys/windows"
)

func TestJobLimitsNone(t *testing.T) {
	info := jobLimits(sandbox.ResourceLimits{}, 0)
	if got := info.BasicLimitInformation.LimitFlags; got != xwin.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE {
		t.Fatalf("LimitFlags = %#x, want KILL_ON_JOB_CLOSE (%#x)", got, xwin.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
	}
	if info.JobMemoryLimit != 0 || info.BasicLimitInformation.PerProcessUserTimeLimit != 0 {
		t.Fatalf("unexpected limits set: memory=%d cpu=%d",
			info.JobMemoryLimit, info.BasicLimitInformation.PerProcessUserTimeLimit)
	}
}

func TestJobLimitsMemory(t *testing.T) {
	info := jobLimits(sandbox.ResourceLimits{MemoryBytes: 64 << 20}, 0)
	flags := info.BasicLimitInformation.LimitFlags
	if flags&xwin.JOB_OBJECT_LIMIT_JOB_MEMORY == 0 {
		t.Fatalf("LimitFlags = %#x, want JOB_OBJECT_LIMIT_JOB_MEMORY set", flags)
	}
	if info.JobMemoryLimit != 64<<20 {
		t.Fatalf("JobMemoryLimit = %d, want %d", info.JobMemoryLimit, 64<<20)
	}
	if info.BasicLimitInformation.PerProcessUserTimeLimit != 0 {
		t.Fatalf("PerProcessUserTimeLimit = %d, want 0", info.BasicLimitInformation.PerProcessUserTimeLimit)
	}
}

func TestJobLimitsCPU(t *testing.T) {
	info := jobLimits(sandbox.ResourceLimits{CPUMillicores: 500}, 2*time.Second)
	flags := info.BasicLimitInformation.LimitFlags
	if flags&xwin.JOB_OBJECT_LIMIT_JOB_TIME == 0 {
		t.Fatalf("LimitFlags = %#x, want JOB_OBJECT_LIMIT_JOB_TIME set", flags)
	}
	// 2s x 500/1000 = 1s budget in 100ns units.
	if got := info.BasicLimitInformation.PerJobUserTimeLimit; got != 10_000_000 {
		t.Fatalf("PerJobUserTimeLimit = %d, want %d", got, 10_000_000)
	}
	if info.JobMemoryLimit != 0 {
		t.Fatalf("JobMemoryLimit = %d, want 0", info.JobMemoryLimit)
	}
}

func TestJobLimitsBoth(t *testing.T) {
	info := jobLimits(sandbox.ResourceLimits{MemoryBytes: 1 << 20, CPUMillicores: 1000}, time.Second)
	flags := info.BasicLimitInformation.LimitFlags
	want := uint32(xwin.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) |
		xwin.JOB_OBJECT_LIMIT_JOB_MEMORY |
		xwin.JOB_OBJECT_LIMIT_JOB_TIME
	if flags != want {
		t.Fatalf("LimitFlags = %#x, want %#x", flags, want)
	}
	if info.JobMemoryLimit != 1<<20 {
		t.Fatalf("JobMemoryLimit = %d, want %d", info.JobMemoryLimit, 1<<20)
	}
	if got := info.BasicLimitInformation.PerJobUserTimeLimit; got != 10_000_000 {
		t.Fatalf("PerJobUserTimeLimit = %d, want %d", got, 10_000_000)
	}
}
