package sandbox

import (
	"context"
	"os/exec"
	"strings"
)

// BwrapConfig controls Bubblewrap sandbox behavior.
// Zero value is usable — all fields have sensible defaults.
type BwrapConfig struct {
	// ShareNet disables network isolation when true (default: isolated).
	ShareNet bool
}

// buildBwrapCommand constructs an exec.Cmd wrapped by bwrap.
//
// Filesystem layout:
//
//	/usr, /bin, /sbin, /lib, /lib64 → read-only host bind
//	/etc/resolv.conf, /etc/ssl      → read-only bind (DNS/TLS, ShareNet only)
//	/proc                           → proc filesystem
//	/dev                            → dev filesystem
//	/tmp                            → tmpfs
//	rootDir                         → read-write bind
//	readOnlyTargets                 → read-only bind (skills/ etc.)
//	readWriteTargets                → read-write bind (data/ etc.)
func buildBwrapCommand(
	ctx context.Context,
	bwrapPath string,
	rootDir string,
	workDir string,
	readOnlyTargets []string,
	readWriteTargets []string,
	cmd string, args []string,
	env []string,
	cfg BwrapConfig,
) *exec.Cmd {
	bwrapArgs := []string{
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind-try", "/sbin", "/sbin",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/lib32", "/lib32",

		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",

		"--bind", rootDir, rootDir,

		"--unshare-user",
		"--unshare-pid",
		"--unshare-uts",
		"--unshare-ipc",

		"--die-with-parent",
		"--chdir", workDir,
	}

	if !cfg.ShareNet {
		bwrapArgs = append(bwrapArgs, "--unshare-net")
	} else {
		bwrapArgs = append(bwrapArgs,
			"--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf",
			"--ro-bind-try", "/etc/ssl", "/etc/ssl",
			"--ro-bind-try", "/etc/ca-certificates", "/etc/ca-certificates",
		)
	}

	for _, target := range readOnlyTargets {
		bwrapArgs = append(bwrapArgs, "--ro-bind-try", target, target)
	}

	for _, target := range readWriteTargets {
		bwrapArgs = append(bwrapArgs, "--bind-try", target, target)
	}

	bwrapArgs = append(bwrapArgs, "--clearenv")
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		bwrapArgs = append(bwrapArgs, "--setenv", k, v)
	}

	bwrapArgs = append(bwrapArgs, "--", cmd)
	bwrapArgs = append(bwrapArgs, args...)

	return exec.CommandContext(ctx, bwrapPath, bwrapArgs...)
}

// minimalEnv builds the minimal environment variable set for bwrap sandboxes.
// Only PATH, HOME, LANG, TERM and explicitly provided extras are included.
func minimalEnv(rootDir string, extra map[string]string) []string {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + rootDir,
		"LANG=C.UTF-8",
		"TERM=dumb",
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
