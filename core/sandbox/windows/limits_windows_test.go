//go:build windows

package windows

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
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

func TestValidatePolicy(t *testing.T) {
	r := &Runner{}
	valid := sandbox.SessionSpec{
		Argv: []string{"cmd.exe"},
		Opts: sandbox.ExecOptions{
			Timeout: time.Second,
			Resources: sandbox.ResourceLimits{
				MemoryBytes:   1 << 20,
				CPUMillicores: 500,
			},
		},
	}
	if err := r.validatePolicy(valid); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}

	cases := []struct {
		name string
		spec sandbox.SessionSpec
	}{
		{
			name: "write read only",
			spec: sandbox.SessionSpec{Opts: sandbox.ExecOptions{Write: sandbox.WriteReadOnly}},
		},
		{
			name: "unknown write policy",
			spec: sandbox.SessionSpec{Opts: sandbox.ExecOptions{Write: sandbox.WritePolicy(99)}},
		},
		{
			name: "net deny all",
			spec: sandbox.SessionSpec{Opts: sandbox.ExecOptions{Net: corenet.NetPolicy{Mode: corenet.NetDenyAll}}},
		},
		{
			name: "disk limit",
			spec: sandbox.SessionSpec{Opts: sandbox.ExecOptions{Resources: sandbox.ResourceLimits{DiskBytes: 1}}},
		},
		{
			name: "cpu without timeout",
			spec: sandbox.SessionSpec{Opts: sandbox.ExecOptions{Resources: sandbox.ResourceLimits{CPUMillicores: 500}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.validatePolicy(tc.spec); !errdefs.IsNotAvailable(err) && !errdefs.IsValidation(err) {
				t.Fatalf("err = %v, want NotAvailable/Validation", err)
			}
		})
	}
}

func TestBuildEnvInheritAll(t *testing.T) {
	got := buildEnv(sandbox.EnvPolicy{})
	if len(got) != len(os.Environ()) {
		t.Fatalf("len = %d, want %d", len(got), len(os.Environ()))
	}
}

func TestBuildEnvInheritNothing(t *testing.T) {
	got := buildEnv(sandbox.EnvPolicy{Allow: []string{}})
	if got == nil || len(got) != 0 {
		t.Fatalf("got = %#v, want empty non-nil slice", got)
	}
}

func TestBuildEnvAllowAndInject(t *testing.T) {
	got := buildEnv(sandbox.EnvPolicy{
		Allow:  []string{"PATH"},
		Inject: map[string]string{"FOO": "bar"},
	})
	seen := map[string]bool{}
	for _, kv := range got {
		key, _, _ := strings.Cut(kv, "=")
		seen[key] = true
	}
	if len(seen) != 2 || !seen["PATH"] || !seen["FOO"] {
		t.Fatalf("keys = %v, want PATH and FOO only", seen)
	}
}
