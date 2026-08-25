package sandbox

import (
	"path/filepath"
	"strings"
)

// ClassifySafeReadOnly reports whether req is a known read-only command
// under the codex-rs-style heuristic: a small set of base commands
// plus argument-aware checks for the commands whose write potential
// depends on their flags (find / rg / git / sed). "sh -c" / "bash -lc"
// wrappers are unwrapped exactly like allowlist matching, so a simple
// `sh -c "ls"` classifies as safe while any composite script falls
// through to false.
//
// The classifier is deliberately conservative: false means "not
// proven read-only", and should route to the human approver, never to
// an automatic deny or allow. It pairs with the per-exec write policy:
//
//	if req.Opts.Write == sandbox.WriteReadOnly &&
//		sandbox.ClassifySafeReadOnly(req.Exec) {
//		return sandbox.Allow, nil
//	}
//
// Like every command predicate, this is a tripwire, not a wall: the
// OS-level read-only enforcement of the backend remains the security
// boundary.
func ClassifySafeReadOnly(req ExecRequest) bool {
	tokens := NormaliseExec(req)
	if len(tokens) == 0 {
		return false
	}
	name := filepath.Base(tokens[0])
	switch name {
	case "find":
		return findIsReadOnly(tokens[1:])
	case "rg", "ripgrep":
		return rgIsReadOnly(tokens[1:])
	case "git":
		return gitIsReadOnly(tokens[1:])
	case "sed":
		return sedIsReadOnly(tokens[1:])
	default:
		return safeReadOnlyBases[name]
	}
}

// safeReadOnlyBases is the set of commands treated as read-only in
// every form. Commands that can execute other programs or write files
// (env, xargs, awk, tar, package managers, compilers, interpreters)
// are deliberately absent: an unrecognized command is a false negative
// that goes to the approver, which is safe.
var safeReadOnlyBases = map[string]bool{
	"basename": true, "cat": true, "cmp": true, "comm": true,
	"cut": true, "date": true, "df": true, "diff": true,
	"dirname": true, "du": true, "echo": true, "file": true,
	"grep": true, "egrep": true, "fgrep": true, "groups": true,
	"head": true, "hexdump": true, "hostname": true, "id": true,
	"less": true, "ls": true, "man": true, "md5sum": true,
	"more": true, "od": true, "printenv": true, "printf": true,
	"pwd": true, "readlink": true, "realpath": true, "sha1sum": true,
	"sha256sum": true, "sort": true, "stat": true, "strings": true,
	"tail": true, "tree": true, "tr": true, "true": true,
	"false": true, "type": true, "uname": true, "uniq": true,
	"wc": true, "which": true, "whoami": true,
}

// findWriteActions are find predicates/actions that write files or
// execute programs. Any occurrence makes the invocation unsafe.
var findWriteActions = map[string]bool{
	"-delete":  true,
	"-exec":    true,
	"-execdir": true,
	"-ok":      true,
	"-okdir":   true,
	"-fls":     true,
	"-fprint":  true,
	"-fprint0": true,
	"-fprintf": true,
}

func findIsReadOnly(args []string) bool {
	for _, arg := range args {
		if findWriteActions[arg] {
			return false
		}
	}
	return true
}

func rgIsReadOnly(args []string) bool {
	for _, arg := range args {
		if arg == "-z" || arg == "--null-data" ||
			arg == "--pre" || arg == "--pre-glob" {
			return false
		}
		if strings.HasPrefix(arg, "--pre=") || strings.HasPrefix(arg, "--pre-glob=") {
			return false
		}
		// Short-flag groups may combine -z with other letters (e.g. -lz).
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") &&
			strings.Contains(arg, "z") {
			return false
		}
	}
	return true
}

// gitSafeSubcommands are git subcommands whose every form is
// read-only. Subcommands with write variants (branch, tag, remote,
// config, stash, reflog, fsck, archive, ...) are absent on purpose.
var gitSafeSubcommands = map[string]bool{
	"blame": true, "cat-file": true, "check-attr": true,
	"check-ignore": true, "check-ref-format": true, "count-objects": true,
	"describe": true, "diff": true, "for-each-ref": true,
	"grep": true, "help": true, "log": true, "ls-files": true,
	"ls-tree": true, "name-rev": true, "rev-parse": true,
	"shortlog": true, "show": true, "status": true,
	"verify-commit": true, "verify-tag": true, "whatchanged": true,
}

// gitValueFlags take a separate value argument and must be skipped
// when locating the subcommand (git -C dir status).
var gitValueFlags = map[string]bool{
	"--exec-path": true, "--git-dir": true, "--namespace": true,
	"--work-tree": true,
}

func gitIsReadOnly(args []string) bool {
	var subcommand string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "--"):
			if gitValueFlags[arg] && i+1 < len(args) {
				i++
			}
			continue
		case strings.HasPrefix(arg, "-"):
			// -C and -c take a value; the rest are boolean flags.
			if (arg == "-C" || arg == "-c") && i+1 < len(args) {
				i++
			}
			continue
		}
		subcommand = arg
		break
	}
	// Bare "git" (or "git --version") prints help/version: read-only.
	return subcommand == "" || gitSafeSubcommands[subcommand]
}

func sedIsReadOnly(args []string) bool {
	scriptSeen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--in-place" || strings.HasPrefix(arg, "-i") {
			return false
		}
		switch arg {
		case "-e", "--expression", "-f", "--file":
			if i+1 < len(args) {
				if sedScriptWrites(args[i+1]) {
					return false
				}
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		// The first positional non-flag argument is the script;
		// the rest are input files.
		if !scriptSeen {
			if sedScriptWrites(arg) {
				return false
			}
			scriptSeen = true
		}
	}
	return true
}

// sedScriptWrites reports whether a sed script contains a write
// command: a 'w' at a command boundary (start of script, after ';' /
// '{' / newline) or as the trailing flag group of a substitution
// ("s/a/b/w file", where the previous character is the delimiter).
func sedScriptWrites(script string) bool {
	for i := 0; i < len(script); i++ {
		if script[i] != 'w' {
			continue
		}
		if i == 0 {
			return true
		}
		switch script[i-1] {
		case ';', '{', '\n', '/', '|', '#', ':':
			return true
		}
	}
	return false
}
