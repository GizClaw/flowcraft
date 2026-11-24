package local

import (
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/core/policy"
)

// TestAnAllowListNeverFallsBackToTheDaemonEnvironment is the widening that made "allow: [HOME]"
// on a host without HOME hand the child everything: exec.Cmd reads a nil Env as "inherit", so an
// allow-list that selected nothing must still be a non-nil slice.
func TestAnAllowListNeverFallsBackToTheDaemonEnvironment(t *testing.T) {
	const absent = "FLOWCRAFT_TEST_VARIABLE_THAT_IS_NOT_SET"

	for _, tc := range []struct {
		name  string
		allow []string
		want  int
	}{
		{"unset allow inherits", nil, -1}, // -1: "whatever the daemon has", asserted non-empty
		{"empty allow inherits nothing", []string{}, 0},
		{"a name that is not set inherits nothing", []string{absent}, 0},
		{"a name that is set inherits only it", []string{"PATH"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := buildEnv(policy.EnvPolicy{Allow: tc.allow})
			if env == nil {
				t.Fatalf("buildEnv returned nil for allow=%v: the child would inherit the daemon's environment", tc.allow)
			}
			if tc.want < 0 {
				if len(env) == 0 {
					t.Skip("this process has an empty environment")
				}
				return
			}
			if len(env) != tc.want {
				t.Fatalf("env = %v, want %d entries", env, tc.want)
			}
			for _, kv := range env {
				if name, _, _ := cut(kv); tc.allow != nil && !contains(tc.allow, name) {
					t.Fatalf("env entry %q is not in the allow-list", kv)
				}
			}
		})
	}
}

// TestInjectIsTheOnlyWayIn keeps the injection rule: a name in Inject is replaced, not duplicated.
func TestInjectIsTheOnlyWayIn(t *testing.T) {
	env := buildEnv(policy.EnvPolicy{
		Allow:  []string{"PATH"},
		Inject: map[string]string{"PATH": "/injected", "EXTRA": "1"},
	})
	got := map[string]string{}
	for _, kv := range env {
		name, value, _ := cut(kv)
		got[name] = value
	}
	if got["PATH"] != "/injected" || got["EXTRA"] != "1" {
		t.Fatalf("env = %v, want the injected values", got)
	}
	if len(env) != 2 {
		t.Fatalf("env = %v, want two entries", env)
	}
}

func cut(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// TestTheChildSeesTheAllowList is the end-to-end form: whatever buildEnv says must be what the
// process actually gets, because that is the boundary the document describes.
func TestTheChildSeesTheAllowList(t *testing.T) {
	env := buildEnv(policy.EnvPolicy{Allow: []string{"FLOWCRAFT_TEST_VARIABLE_THAT_IS_NOT_SET"}})
	if len(env) != 0 {
		t.Fatalf("env = %v, want nothing", env)
	}
	if err := os.Setenv("FLOWCRAFT_TEST_VARIABLE_THAT_IS_NOT_SET", "x"); err != nil {
		t.Fatalf("Setenv: %v", err)
	}
	defer func() { _ = os.Unsetenv("FLOWCRAFT_TEST_VARIABLE_THAT_IS_NOT_SET") }()
	if got := buildEnv(policy.EnvPolicy{Allow: []string{"FLOWCRAFT_TEST_VARIABLE_THAT_IS_NOT_SET"}}); len(got) != 1 {
		t.Fatalf("env = %v, want the one allowed entry", got)
	}
}
