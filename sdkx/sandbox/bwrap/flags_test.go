package bwrap

import (
	"sort"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestBuildFlags_BaseAlwaysIncluded(t *testing.T) {
	got, err := buildFlags(sandbox.ExecOptions{}, nil)
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	for _, want := range []string{"--die-with-parent", "--unshare-pid", "--clearenv", "--share-net"} {
		if !contains(got, want) {
			t.Errorf("missing base flag %q in %v", want, got)
		}
	}
	if contains(got, "--unshare-net") {
		t.Errorf("zero-value NetPolicy must inherit host net, got %v", got)
	}
}

func TestFilesystemFlags(t *testing.T) {
	got := filesystemFlags("/workspace", []string{"/cache", "/workspace"})
	for key, value := range map[string]string{
		"--ro-bind": "/",
		"--tmpfs":   "/tmp",
		"--bind":    "/workspace",
		"--proc":    "/proc",
		"--dev":     "/dev",
	} {
		if !hasPair(got, key, value) {
			t.Errorf("missing %s %s in %v", key, value, got)
		}
	}
	if !hasPair(got, "--bind", "/cache") {
		t.Errorf("missing writable exception in %v", got)
	}
	if countPair(got, "--bind", "/workspace") != 1 {
		t.Errorf("root bind should be emitted once, got %v", got)
	}
	if indexPair(got, "--tmpfs", "/tmp") > indexPair(got, "--bind", "/workspace") {
		t.Errorf("private /tmp must mount before a workspace nested under it: %v", got)
	}
	if indexPair(got, "--proc", "/proc") < indexPair(got, "--bind", "/workspace") {
		t.Errorf("/proc should mount after the workspace bind: %v", got)
	}
}

func TestBuildFlags_WorkDir(t *testing.T) {
	got, err := buildFlags(sandbox.ExecOptions{WorkDir: "/work"}, nil)
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	if !hasPair(got, "--chdir", "/work") {
		t.Errorf("expected --chdir /work, got %v", got)
	}
}

func TestBuildFlags_WorkDirEmpty(t *testing.T) {
	got, err := buildFlags(sandbox.ExecOptions{}, nil)
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	if contains(got, "--chdir") {
		t.Errorf("did not expect --chdir when WorkDir empty, got %v", got)
	}
}

func TestEnvFlags_NilAllowInheritsAll(t *testing.T) {
	host := []string{"PATH=/usr/bin", "HOME=/root", "LANG=C"}
	got := envFlags(sandbox.EnvPolicy{}, host)
	if got[0] != "--clearenv" {
		t.Fatalf("expected leading --clearenv, got %v", got)
	}
	envs := extractEnvAssignments(got)
	want := []string{"HOME=/root", "LANG=C", "PATH=/usr/bin"}
	if !equalSorted(envs, want) {
		t.Errorf("nil Allow: expected %v, got %v", want, envs)
	}
}

func TestEnvFlags_EmptyAllowDropsHost(t *testing.T) {
	host := []string{"PATH=/usr/bin", "HOME=/root"}
	got := envFlags(sandbox.EnvPolicy{Allow: []string{}}, host)
	if len(got) != 1 || got[0] != "--clearenv" {
		t.Errorf("empty Allow: expected just --clearenv, got %v", got)
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
		name     string
		mode     sandbox.NetMode
		wantFlag string
	}{
		{name: "default_inherits_host", mode: sandbox.NetDefault, wantFlag: "--share-net"},
		{name: "deny_all_isolates", mode: sandbox.NetDenyAll, wantFlag: "--unshare-net"},
		{name: "allow_list_isolates", mode: sandbox.NetAllowList, wantFlag: "--unshare-net"},
		{name: "proxy_isolates", mode: sandbox.NetProxy, wantFlag: "--unshare-net"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := netFlags(sandbox.NetPolicy{Mode: tc.mode})
			if err != nil {
				t.Fatalf("netFlags: %v", err)
			}
			if len(got) != 1 || got[0] != tc.wantFlag {
				t.Errorf("expected %q, got %v", tc.wantFlag, got)
			}
		})
	}
}

func TestNetIsolationFlags(t *testing.T) {
	if got := netIsolationFlags(sandbox.NetDefault); got != nil {
		t.Errorf("NetDefault must keep host /run, got %v", got)
	}
	for _, mode := range []sandbox.NetMode{sandbox.NetDenyAll, sandbox.NetAllowList, sandbox.NetProxy} {
		got := netIsolationFlags(mode)
		if len(got) != 2 || got[0] != "--tmpfs" || got[1] != "/run" {
			t.Errorf("mode %d: expected --tmpfs /run, got %v", int(mode), got)
		}
	}
}

func TestBuildFlags_Combined(t *testing.T) {
	got, err := buildFlags(sandbox.ExecOptions{
		WorkDir: "/work",
		Env: sandbox.EnvPolicy{
			Allow:  []string{"PATH"},
			Inject: map[string]string{"RUN_ID": "z"},
		},
		Net: sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
	}, []string{"PATH=/usr/bin", "HOME=/root", "SECRET=shh"})
	if err != nil {
		t.Fatalf("buildFlags: %v", err)
	}
	if !hasPair(got, "--chdir", "/work") {
		t.Errorf("missing --chdir /work in %v", got)
	}
	if !contains(got, "--unshare-net") {
		t.Errorf("missing --unshare-net in %v", got)
	}
	if contains(got, "--share-net") {
		t.Errorf("NetDenyAll should NOT carry --share-net, got %v", got)
	}
	envs := extractEnvAssignments(got)
	if !equalSorted(envs, []string{"PATH=/usr/bin", "RUN_ID=z"}) {
		t.Errorf("env mismatch: got %v", envs)
	}
	if strings.Contains(strings.Join(got, " "), "SECRET") || strings.Contains(strings.Join(got, " "), "HOME=") {
		t.Errorf("host env leaked through allow-list: %v", got)
	}
}

func TestValidateExtraFlags_RejectsWeakeningFlags(t *testing.T) {
	for _, flag := range []string{
		"--ro-bind", "--bind", "--dev-bind", "--bind-try", "--ro-bind-try",
		"--bind-fd", "--ro-bind-fd", "--remount-ro",
		"--overlay", "--tmp-overlay", "--ro-overlay", "--overlay-src",
		"--proc", "--dev", "--tmpfs", "--mqueue", "--dir", "--file",
		"--bind-data", "--ro-bind-data", "--symlink", "--chmod", "--perms",
		"--clearenv", "--setenv", "--unsetenv",
		"--unshare-all", "--share-net", "--unshare-user", "--unshare-user-try",
		"--unshare-ipc", "--unshare-pid", "--unshare-net", "--unshare-uts",
		"--unshare-cgroup", "--unshare-cgroup-try", "--disable-userns",
		"--assert-userns-disabled", "--userns", "--userns2", "--pidns",
		"--chdir", "--uid", "--gid", "--hostname",
		"--seccomp", "--add-seccomp-fd", "--cap-add", "--cap-drop",
		"--new-session", "--args",
		"--ro-bind=/x:/y", "--setenv=PATH=/x", "--unshare-net",
	} {
		t.Run(strings.TrimLeft(flag, "-"), func(t *testing.T) {
			err := validateExtraFlags([]string{flag})
			if !errdefs.IsValidation(err) {
				t.Fatalf("flag %q: expected Validation, got %v", flag, err)
			}
		})
	}
}

func TestValidateExtraFlags_AllowsBenignFlags(t *testing.T) {
	for _, flag := range []string{
		"--die-with-parent",
		"--level-prefix", "--argv0", "--info-fd", "--json-status-fd",
		"--lock-file", "--sync-fd", "--block-fd",
	} {
		t.Run(strings.TrimLeft(flag, "-"), func(t *testing.T) {
			if err := validateExtraFlags([]string{flag}); err != nil {
				t.Fatalf("flag %q should be allowed, got %v", flag, err)
			}
		})
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

func countPair(args []string, key, value string) int {
	count := 0
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			count++
		}
	}
	return count
}

func indexPair(args []string, key, value string) int {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return i
		}
	}
	return -1
}

func extractEnvAssignments(args []string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--setenv" && i+2 < len(args) {
			out = append(out, args[i+1]+"="+args[i+2])
			i += 2
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
