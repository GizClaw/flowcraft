//go:build windows

package windows

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

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
	denyAll := valid
	denyAll.Opts.Net = permission.NetPolicy{Mode: permission.NetDenyAll}
	if err := r.validatePolicy(denyAll); err != nil {
		t.Fatalf("net deny-all policy rejected: %v", err)
	}

	cases := []struct {
		name string
		spec sandbox.SessionSpec
	}{
		{
			name: "write read only",
			spec: sandbox.SessionSpec{Opts: sandbox.ExecOptions{Permission: &permission.Policy{}}},
		},
		{
			name: "unknown write policy",
			spec: sandbox.SessionSpec{Opts: sandbox.ExecOptions{Permission: &permission.Policy{Writes: permission.Writes{Paths: []string{"relative"}}}}},
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
