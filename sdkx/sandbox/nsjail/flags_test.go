package nsjail

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestBuildFlags_BaseAlwaysIncluded(t *testing.T) {
	got, err := buildFlags(sandbox.ExecOptions{}, nil)
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	for _, want := range []string{"-Mo", "--quiet", "--disable_clone_newns"} {
		if !contains(got, want) {
			t.Errorf("missing base flag %q in %v", want, got)
		}
	}
}

func TestBuildFlags_WorkDir(t *testing.T) {
	got, err := buildFlags(sandbox.ExecOptions{WorkDir: "/work"}, nil)
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	if !hasPair(got, "--cwd", "/work") {
		t.Errorf("expected --cwd /work, got %v", got)
	}
}

func TestBuildFlags_WorkDirEmpty(t *testing.T) {
	got, err := buildFlags(sandbox.ExecOptions{}, nil)
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	if contains(got, "--cwd") {
		t.Errorf("did not expect --cwd when WorkDir empty, got %v", got)
	}
}

func TestBuildFlags_Timeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want string
	}{
		{"two_seconds", 2 * time.Second, "2"},
		{"sub_second_rounds_up_to_one", 100 * time.Millisecond, "1"},
		{"fractional_seconds_round_up", 1500 * time.Millisecond, "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildFlags(sandbox.ExecOptions{Timeout: tc.in}, nil)
			if err != nil {
				t.Fatalf("buildFlags: %v", err)
			}
			if !hasPair(got, "--time_limit", tc.want) {
				t.Errorf("expected --time_limit %s, got %v", tc.want, got)
			}
		})
	}
}

func TestBuildFlags_TimeoutZeroOmitted(t *testing.T) {
	got, err := buildFlags(sandbox.ExecOptions{}, nil)
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	if contains(got, "--time_limit") {
		t.Errorf("did not expect --time_limit when Timeout zero, got %v", got)
	}
}

func TestEnvFlags_NilAllowInheritsAll(t *testing.T) {
	host := []string{"PATH=/usr/bin", "HOME=/root", "LANG=C"}
	got := envFlags(sandbox.EnvPolicy{}, host)
	envs := extractEnvAssignments(got)
	want := []string{"HOME=/root", "LANG=C", "PATH=/usr/bin"}
	if !equalSorted(envs, want) {
		t.Errorf("nil Allow: expected %v, got %v", want, envs)
	}
}

func TestEnvFlags_EmptyAllowDropsHost(t *testing.T) {
	host := []string{"PATH=/usr/bin", "HOME=/root"}
	got := envFlags(sandbox.EnvPolicy{Allow: []string{}}, host)
	if len(got) != 0 {
		t.Errorf("empty Allow: expected no flags, got %v", got)
	}
}

func TestEnvFlags_ExplicitAllowFiltersHost(t *testing.T) {
	host := []string{"PATH=/usr/bin", "HOME=/root", "SECRET=shh"}
	got := envFlags(sandbox.EnvPolicy{Allow: []string{"PATH", "HOME", "UNSET"}}, host)
	envs := extractEnvAssignments(got)
	want := []string{"HOME=/root", "PATH=/usr/bin"}
	if !equalSorted(envs, want) {
		t.Errorf("allow filter: expected %v, got %v", want, envs)
	}
}

func TestEnvFlags_InjectAddsAndOverrides(t *testing.T) {
	host := []string{"PATH=/usr/bin", "HOME=/root"}
	got := envFlags(sandbox.EnvPolicy{
		Allow:  []string{"PATH"},
		Inject: map[string]string{"PATH": "/sandbox/bin", "RUN_ID": "abc"},
	}, host)
	envs := extractEnvAssignments(got)
	want := []string{"PATH=/sandbox/bin", "RUN_ID=abc"}
	if !equalSorted(envs, want) {
		t.Errorf("inject override: expected %v, got %v", want, envs)
	}
}

func TestEnvFlags_InjectWithNilAllow(t *testing.T) {
	host := []string{"PATH=/usr/bin"}
	got := envFlags(sandbox.EnvPolicy{
		Inject: map[string]string{"RUN_ID": "xyz"},
	}, host)
	envs := extractEnvAssignments(got)
	want := []string{"PATH=/usr/bin", "RUN_ID=xyz"}
	if !equalSorted(envs, want) {
		t.Errorf("nil allow + inject: expected %v, got %v", want, envs)
	}
}

func TestNetFlags(t *testing.T) {
	cases := []struct {
		name      string
		mode      sandbox.NetMode
		wantFlag  string
		wantNoNet bool
		wantErr   bool
	}{
		{name: "default_inherits_host", mode: sandbox.NetDefault, wantFlag: "--disable_clone_newnet"},
		{name: "deny_all_uses_default_namespace", mode: sandbox.NetDenyAll, wantNoNet: true},
		{name: "allow_list_unimplemented", mode: sandbox.NetAllowList, wantErr: true},
		{name: "proxy_unimplemented", mode: sandbox.NetProxy, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := netFlags(sandbox.NetPolicy{Mode: tc.mode})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got flags %v", got)
				}
				if !errdefs.IsNotAvailable(err) {
					t.Errorf("expected NotAvailable, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantNoNet {
				if contains(got, "--disable_clone_newnet") {
					t.Errorf("did not expect --disable_clone_newnet, got %v", got)
				}
				return
			}
			if !contains(got, tc.wantFlag) {
				t.Errorf("expected %q in %v", tc.wantFlag, got)
			}
		})
	}
}

func TestResourceFlags(t *testing.T) {
	t.Run("cpu_passthrough", func(t *testing.T) {
		got, err := resourceFlags(sandbox.ResourceLimits{CPUMillicores: 500})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !hasPair(got, "--cgroup_cpu_ms_per_sec", "500") {
			t.Errorf("expected --cgroup_cpu_ms_per_sec 500, got %v", got)
		}
	})
	t.Run("memory_passthrough", func(t *testing.T) {
		got, err := resourceFlags(sandbox.ResourceLimits{MemoryBytes: 256 << 20})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !hasPair(got, "--cgroup_mem_max", "268435456") {
			t.Errorf("expected --cgroup_mem_max 268435456, got %v", got)
		}
	})
	t.Run("disk_unimplemented", func(t *testing.T) {
		_, err := resourceFlags(sandbox.ResourceLimits{DiskBytes: 1024})
		if err == nil || !errdefs.IsNotAvailable(err) {
			t.Errorf("expected NotAvailable, got %v", err)
		}
	})
	t.Run("max_output_bytes_is_in_process", func(t *testing.T) {
		got, err := resourceFlags(sandbox.ResourceLimits{MaxOutputBytes: 1 << 20})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("MaxOutputBytes should not emit nsjail flags, got %v", got)
		}
	})
	t.Run("zero_emits_nothing", func(t *testing.T) {
		got, err := resourceFlags(sandbox.ResourceLimits{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("zero ResourceLimits should emit no flags, got %v", got)
		}
	})
}

func TestBuildFlags_RejectsUnsupportedNet(t *testing.T) {
	_, err := buildFlags(sandbox.ExecOptions{
		Net: sandbox.NetPolicy{Mode: sandbox.NetAllowList},
	}, nil)
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable for NetAllowList, got %v", err)
	}
}

func TestBuildFlags_RejectsDiskBytes(t *testing.T) {
	_, err := buildFlags(sandbox.ExecOptions{
		Resources: sandbox.ResourceLimits{DiskBytes: 1024},
	}, nil)
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable for DiskBytes, got %v", err)
	}
}

func TestBuildFlags_EndToEnd(t *testing.T) {
	host := []string{"PATH=/usr/bin"}
	got, err := buildFlags(sandbox.ExecOptions{
		WorkDir: "/w",
		Timeout: 3 * time.Second,
		Env: sandbox.EnvPolicy{
			Allow:  []string{"PATH"},
			Inject: map[string]string{"RUN_ID": "z"},
		},
		Net: sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
		Resources: sandbox.ResourceLimits{
			CPUMillicores: 1000,
			MemoryBytes:   1 << 30,
		},
	}, host)
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	if !hasPair(got, "--cwd", "/w") {
		t.Errorf("missing --cwd /w in %v", got)
	}
	if !hasPair(got, "--time_limit", "3") {
		t.Errorf("missing --time_limit 3 in %v", got)
	}
	envs := extractEnvAssignments(got)
	if !equalSorted(envs, []string{"PATH=/usr/bin", "RUN_ID=z"}) {
		t.Errorf("env mismatch: got %v", envs)
	}
	if contains(got, "--disable_clone_newnet") {
		t.Errorf("NetDenyAll should NOT carry --disable_clone_newnet, got %v", got)
	}
	if !hasPair(got, "--cgroup_cpu_ms_per_sec", "1000") {
		t.Errorf("missing CPU cap in %v", got)
	}
	if !hasPair(got, "--cgroup_mem_max", "1073741824") {
		t.Errorf("missing memory cap in %v", got)
	}
}

// --- helpers ---

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func hasPair(args []string, key, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func extractEnvAssignments(args []string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--env" && strings.ContainsRune(args[i+1], '=') {
			out = append(out, args[i+1])
		}
	}
	return out
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}
