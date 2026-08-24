//go:build windows

package windows

import (
	"fmt"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// buildEnv translates a sandbox.EnvPolicy into a flat []string
// suitable for exec.Cmd.Env, with the same semantics as the local and
// seatbelt backends: Allow == nil inherits the host environment,
// Allow == []string{} inherits nothing, and Inject wins over host
// values of the same name. The empty result is returned as []string{}
// so os/exec does not fall back to inheriting everything.
//
// Windows note: exec.Cmd renders this slice into the UTF-16 double
// null-terminated environment block itself. The restricted-token
// spawn path must not re-inherit the parent block; anything not on
// the allow-list is dropped here, before the child ever sees it.
func buildEnv(p sandbox.EnvPolicy) []string {
	var env []string

	if p.Allow == nil {
		env = append(env, os.Environ()...)
	} else if len(p.Allow) > 0 {
		allow := make(map[string]bool, len(p.Allow))
		for _, name := range p.Allow {
			allow[name] = true
		}
		for _, kv := range os.Environ() {
			idx := strings.IndexByte(kv, '=')
			if idx <= 0 {
				continue
			}
			if allow[kv[:idx]] {
				env = append(env, kv)
			}
		}
	}

	if len(p.Inject) > 0 {
		injected := make(map[string]bool, len(p.Inject))
		for k := range p.Inject {
			injected[k] = true
		}
		filtered := env[:0]
		for _, kv := range env {
			idx := strings.IndexByte(kv, '=')
			if idx <= 0 {
				filtered = append(filtered, kv)
				continue
			}
			if !injected[kv[:idx]] {
				filtered = append(filtered, kv)
			}
		}
		env = filtered
		for k, v := range p.Inject {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	if p.Allow != nil && len(p.Allow) == 0 && len(p.Inject) == 0 {
		return []string{}
	}
	return env
}
