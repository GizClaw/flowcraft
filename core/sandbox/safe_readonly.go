package sandbox

import (
	"path/filepath"
	"strings"
)

// ClassifySafeReadOnly reports whether req is a known read-only command
// under the codex-rs-style heuristic: a small set of base commands
// plus argument-aware checks for the commands whose write potential
// depends on their flags (find / rg / git / sed / sort). "sh -c" /
// "bash -lc" wrappers are unwrapped exactly like allowlist matching,
// so a simple `sh -c "ls"` classifies as safe while any composite
// script falls through to false.
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
	case "sort":
		return sortIsReadOnly(tokens[1:])
	default:
		return safeReadOnlyBases[name]
	}
}

// safeReadOnlyBases is the set of commands treated as read-only in
// every form. Commands that can execute other programs or write files
// (env, xargs, awk, tar, package managers, compilers, interpreters)
// are deliberately absent: an unrecognized command is a false negative
// that goes to the approver, which is safe. date and hostname are also
// absent: `date -s` changes the system clock and `hostname newname`
// changes the host name — non-file state writes that neither seatbelt
// nor bwrap can block, so they must never auto-approve (codex-rs
// omits them for the same reason).
var safeReadOnlyBases = map[string]bool{
	"basename": true, "cat": true, "cmp": true, "comm": true,
	"cut": true, "df": true, "diff": true,
	"dirname": true, "du": true, "echo": true, "file": true,
	"grep": true, "egrep": true, "fgrep": true, "groups": true,
	"head": true, "hexdump": true, "id": true,
	"less": true, "ls": true, "man": true, "md5sum": true,
	"more": true, "od": true, "printenv": true, "printf": true,
	"pwd": true, "readlink": true, "realpath": true, "sha1sum": true,
	"sha256sum": true, "stat": true, "strings": true, "tail": true,
	"tree": true, "tr": true, "true": true, "false": true,
	"type": true, "uname": true, "uniq": true, "wc": true,
	"which": true, "whoami": true,
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

// sortIsReadOnly rejects output redirection (`-o`, `--output`) in any
// form, including short-flag groups like `sort -ro out`, so the sorted
// result cannot be written to a file.
func sortIsReadOnly(args []string) bool {
	for _, arg := range args {
		if arg == "-o" || arg == "--output" || strings.HasPrefix(arg, "--output=") {
			return false
		}
		// -o is the only sort short flag containing 'o'; a group like
		// -ro is equivalent to -r -o and must be treated the same way.
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") &&
			strings.Contains(arg, "o") {
			return false
		}
	}
	return true
}

// rgIsReadOnly rejects the ripgrep options that can execute arbitrary
// commands (--pre, --pre-glob, --hostname-bin) or call out to other
// decompression tools (--search-zip, -z), mirroring codex-rs's fix for
// CVE-2025-54558. Short-flag groups may combine -z with other letters
// (e.g. -lz), so any single-dash group containing 'z' is rejected too.
func rgIsReadOnly(args []string) bool {
	for _, arg := range args {
		if arg == "-z" || arg == "--null-data" ||
			arg == "--pre" || arg == "--pre-glob" ||
			arg == "--search-zip" || arg == "--hostname-bin" {
			return false
		}
		if strings.HasPrefix(arg, "--pre=") || strings.HasPrefix(arg, "--pre-glob=") ||
			strings.HasPrefix(arg, "--hostname-bin=") {
			return false
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") &&
			strings.Contains(arg, "z") {
			return false
		}
	}
	return true
}

// gitSafeSubcommands are git subcommands whose every form is
// read-only once their options are checked (see gitWriteVariantFlags).
// Subcommands with write variants (branch, tag, remote, config, stash,
// reflog, fsck, archive, ...) are absent on purpose.
var gitSafeSubcommands = map[string]bool{
	"blame": true, "cat-file": true, "check-attr": true,
	"check-ignore": true, "check-ref-format": true, "count-objects": true,
	"describe": true, "diff": true, "for-each-ref": true,
	"grep": true, "help": true, "log": true, "ls-files": true,
	"ls-tree": true, "name-rev": true, "rev-parse": true,
	"shortlog": true, "show": true, "status": true,
	"verify-commit": true, "verify-tag": true, "whatchanged": true,
}

// gitWriteVariantFlags are diff/log/show options that write the output
// to a file (--output) or run external programs (--ext-diff, --textconv,
// --exec); any occurrence after the subcommand makes the invocation
// unsafe. Aligned with codex-rs's UNSAFE_GIT_SUBCOMMAND_OPTIONS.
var gitWriteVariantFlags = []string{
	"--output", "--ext-diff", "--textconv", "--exec",
}

// gitValueFlags take a separate value argument and must be skipped
// when locating the subcommand (git -C dir status).
var gitValueFlags = map[string]bool{
	"--exec-path": true, "--git-dir": true, "--namespace": true,
	"--work-tree": true,
}

func gitIsReadOnly(args []string) bool {
	var subcommand string
	rest := args
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
		rest = args[i+1:]
		break
	}
	// Bare "git" (or "git --version") prints help/version: read-only.
	if subcommand == "" {
		return true
	}
	if !gitSafeSubcommands[subcommand] {
		return false
	}
	for _, arg := range rest {
		if arg == "--output" || strings.HasPrefix(arg, "--output=") {
			return false
		}
		for _, flag := range gitWriteVariantFlags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") {
				return false
			}
		}
	}
	return true
}

// sedIsReadOnly follows codex-rs: only `sed -n <addr>p [file]` is
// auto-approved, where <addr> is `N` or `N,M` of decimal digits.
// Every other script form — substitution, `w` writes (including
// address-form `sed '1w file'` / `sed '2,5w file'`), `-e`, `-i`,
// multiple files — routes to the approver, since a conservative
// script parser cannot prove them write-free.
func sedIsReadOnly(args []string) bool {
	if len(args) < 2 || len(args) > 3 {
		return false
	}
	if args[0] != "-n" {
		return false
	}
	return isSedPrintAddress(args[1])
}

// isSedPrintAddress reports whether s is a decimal line-address range
// ending in `p` (`Np` or `N,Mp`), matching codex-rs's /^(\d+,)?\d+p$/.
func isSedPrintAddress(s string) bool {
	core, ok := strings.CutSuffix(s, "p")
	if !ok || core == "" {
		return false
	}
	if !strings.Contains(core, ",") {
		return allDigits(core)
	}
	parts := strings.Split(core, ",")
	if len(parts) != 2 {
		return false
	}
	return parts[0] != "" && parts[1] != "" &&
		allDigits(parts[0]) && allDigits(parts[1])
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
