package nsjail

import (
	"math"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// buildFlags translates sandbox.ExecOptions into the nsjail CLI flag
// list that precedes the "--" separator. The command and its arguments
// are appended by the caller, not by this function.
//
// hostEnv mirrors os.Environ() output ("KEY=VALUE" pairs) and is
// injected by the caller so the translation stays pure for testing.
// The function returns errdefs.NotAvailable for policy fields nsjail
// cannot enforce in this version (NetAllowList, NetProxy, DiskBytes)
// — matching the Runner contract that unknown / unsupported policy
// must fail loudly, never be silently dropped.
func buildFlags(opts sandbox.ExecOptions, hostEnv []string) ([]string, error) {
	flags := []string{
		"-Mo",
		"--quiet",
		// --disable_clone_newns: keep the host filesystem visible;
		// WorkDir confinement is handled in Go. See doc.go for the
		// rationale (full mount-ns isolation is a future RFC).
		"--disable_clone_newns",
		// --disable_clone_newuts: nsjail's default UTS-namespace
		// behaviour invokes sethostname("NSJAIL"), which needs
		// CAP_SYS_ADMIN in the new namespace. Unprivileged user-ns
		// mappings on hosts that haven't enabled
		// kernel.unprivileged_userns_clone the "permissive" way
		// (notably GitHub Actions runners) refuse the sethostname
		// call and the child fails to launch. The MVP does not rely
		// on hostname isolation as a security boundary, so we opt
		// out of UTS-namespace cloning entirely.
		"--disable_clone_newuts",
	}

	if opts.WorkDir != "" {
		flags = append(flags, "--cwd", opts.WorkDir)
	}

	if opts.Timeout > 0 {
		secs := int64(math.Ceil(opts.Timeout.Seconds()))
		if secs < 1 {
			secs = 1
		}
		flags = append(flags, "--time_limit", strconv.FormatInt(secs, 10))
	}

	flags = append(flags, envFlags(opts.Env, hostEnv)...)

	netF, err := netFlags(opts.Net)
	if err != nil {
		return nil, err
	}
	flags = append(flags, netF...)

	resF, err := resourceFlags(opts.Resources)
	if err != nil {
		return nil, err
	}
	flags = append(flags, resF...)

	return flags, nil
}

// envFlags renders sandbox.EnvPolicy as a sequence of --env KEY=VALUE
// arguments. We do not use nsjail's --keep_env because it asks nsjail
// to read its own host env at child-spawn time, which would couple
// behaviour to how the caller invokes nsjail (e.g. via su / sudo).
// Snapshotting on this side keeps the policy interpretation identical
// to LocalRunner.buildEnv: Allow filters host vars, Inject layers on
// top with override semantics.
func envFlags(p sandbox.EnvPolicy, hostEnv []string) []string {
	keep := map[string]string{}

	switch {
	case p.Allow == nil:
		for _, kv := range hostEnv {
			if name, value, ok := splitKV(kv); ok {
				keep[name] = value
			}
		}
	case len(p.Allow) > 0:
		allow := make(map[string]bool, len(p.Allow))
		for _, name := range p.Allow {
			allow[name] = true
		}
		for _, kv := range hostEnv {
			if name, value, ok := splitKV(kv); ok && allow[name] {
				keep[name] = value
			}
		}
	}

	for k, v := range p.Inject {
		keep[k] = v
	}

	if len(keep) == 0 {
		return nil
	}
	out := make([]string, 0, 2*len(keep))
	for k, v := range keep {
		out = append(out, "--env", k+"="+v)
	}
	return out
}

func splitKV(kv string) (string, string, bool) {
	i := strings.IndexByte(kv, '=')
	if i <= 0 {
		return "", "", false
	}
	return kv[:i], kv[i+1:], true
}

func netFlags(p sandbox.NetPolicy) ([]string, error) {
	switch p.Mode {
	case sandbox.NetDefault:
		// Inherit the host's network namespace.
		return []string{"--disable_clone_newnet"}, nil
	case sandbox.NetDenyAll:
		// nsjail's default clones a fresh net namespace with only lo.
		return nil, nil
	case sandbox.NetAllowList:
		return nil, errdefs.NotAvailablef(
			"nsjail: net allow-list not yet implemented (requires iptables / nftables integration)",
		)
	case sandbox.NetProxy:
		return nil, errdefs.NotAvailablef(
			"nsjail: net proxy mode not yet implemented",
		)
	default:
		return nil, errdefs.NotAvailablef("nsjail: unknown net mode %d", int(p.Mode))
	}
}

func resourceFlags(r sandbox.ResourceLimits) ([]string, error) {
	if r.DiskBytes != 0 {
		return nil, errdefs.NotAvailablef(
			"nsjail: disk quota not yet implemented (requires tmpfs mount integration)",
		)
	}
	var out []string
	if r.CPUMillicores > 0 {
		// nsjail --cgroup_cpu_ms_per_sec: ms of CPU time per real second.
		// 1000 == one full core. Caller's CPUMillicores already uses the
		// same unit, so it is a direct pass-through.
		out = append(out, "--cgroup_cpu_ms_per_sec",
			strconv.FormatInt(int64(r.CPUMillicores), 10))
	}
	if r.MemoryBytes > 0 {
		out = append(out, "--cgroup_mem_max",
			strconv.FormatInt(r.MemoryBytes, 10))
	}
	return out, nil
}
