package sandbox

import (
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// rule is one parsed allowlist entry: a token prefix plus an optional
// trailing wildcard.
type rule struct {
	tokens   []string
	wildcard bool
}

// Allowlist is a thread-safe set of command rules used to decide which
// Exec calls are in-bounds (no human approval needed). A rule is a
// whitespace-separated token prefix of the normalized command, with an
// optional trailing "*" that matches any remaining arguments; without
// "*" the rule matches the normalized command exactly:
//
//	go run *     matches "go run main.go", "go run test", and "go run"
//	go *         matches any "go" invocation
//	git status   matches only "git status"
//
// The first token is matched against the command's base name as well as
// its literal form, so "/usr/bin/go" satisfies the rule "go *". Rules
// are intended to be assembled from layered configuration (defaults +
// project overrides via NewAllowlist/Add/Union) and mutated at runtime
// (Add/Set) while Exec calls are in flight.
//
// Allowlist matching is deliberately heuristic: NormaliseExec unwraps
// the shell wrappers hosts put around shell-syntax commands ("sh -c",
// "cmd /c", "pwsh -NoProfile -Command") when the script is provably a
// single simple command. Everything else — control characters,
// expansions, unrecognised wrapper forms — stays raw and falls through
// to the approver. Like every predicate, the allowlist is the tripwire,
// not the wall — OS-level backend enforcement remains the security
// boundary.
type Allowlist struct {
	mu    sync.RWMutex
	rules []rule
}

// NewAllowlist builds an allowlist from rules. Invalid rules abort the
// construction; the returned list is nil.
func NewAllowlist(rules ...string) (*Allowlist, error) {
	a := &Allowlist{}
	if err := a.Add(rules...); err != nil {
		return nil, err
	}
	return a, nil
}

// Add appends rules. Add is idempotent (duplicate rules are ignored)
// and all rules are validated before any is applied, so a single
// invalid rule leaves the allowlist unchanged.
func (a *Allowlist) Add(rules ...string) error {
	if a == nil {
		return errdefs.Validationf("sandbox allowlist: nil Allowlist")
	}
	parsed := make([]rule, 0, len(rules))
	for _, raw := range rules {
		r, err := parseRule(raw)
		if err != nil {
			return err
		}
		parsed = append(parsed, r)
	}
	a.mu.Lock()
	seen := make(map[string]bool, len(a.rules)+len(parsed))
	for _, r := range a.rules {
		seen[r.String()] = true
	}
	for _, r := range parsed {
		if seen[r.String()] {
			continue
		}
		seen[r.String()] = true
		a.rules = append(a.rules, r)
	}
	a.mu.Unlock()
	return nil
}

// Set replaces the whole rule set (config reload). All rules are
// validated before the replacement is applied.
func (a *Allowlist) Set(rules []string) error {
	if a == nil {
		return errdefs.Validationf("sandbox allowlist: nil Allowlist")
	}
	parsed := make([]rule, 0, len(rules))
	for _, raw := range rules {
		r, err := parseRule(raw)
		if err != nil {
			return err
		}
		parsed = append(parsed, r)
	}
	a.mu.Lock()
	a.rules = parsed
	a.mu.Unlock()
	return nil
}

// Union merges other's rules into a without duplicating entries. It is
// the composition primitive for layered configuration (defaults ∪
// project overrides).
func (a *Allowlist) Union(other *Allowlist) error {
	if other == nil {
		return nil
	}
	seen := make(map[string]bool, len(a.Rules()))
	for _, raw := range a.Rules() {
		seen[raw] = true
	}
	var add []string
	for _, raw := range other.Rules() {
		if !seen[raw] {
			seen[raw] = true
			add = append(add, raw)
		}
	}
	return a.Add(add...)
}

// Rules returns a snapshot of the current rule strings, suitable for
// persisting the effective list back into config.
func (a *Allowlist) Rules() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.rules))
	for _, r := range a.rules {
		out = append(out, r.String())
	}
	return out
}

// Matches reports whether req is in-bounds according to the rules.
func (a *Allowlist) Matches(req ExecRequest) bool {
	if a == nil {
		return false
	}
	tokens := NormaliseExec(req)
	if len(tokens) == 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, r := range a.rules {
		if matchRule(r, tokens) {
			return true
		}
	}
	return false
}

// NotAllowed returns a Predicate that matches calls outside the
// allowlist, so WithApproval routes them to the approver (or fails
// closed when no approver is configured).
func (a *Allowlist) NotAllowed() Predicate {
	return PredicateFunc(func(req ExecRequest) (string, bool) {
		if a != nil && a.Matches(req) {
			return "", false
		}
		return "command not in sandbox allowlist", true
	})
}

// NormaliseExec returns the token list used for allowlist matching.
// A shell invocation ("sh -c <script>", "cmd /c <script>",
// "pwsh -NoProfile -Command <script>") is unwrapped to the script's
// tokens when the wrapper form is exactly the supported one and the
// script is provably a single simple command under that shell's
// grammar. Complex scripts (control characters, substitutions,
// redirects, expansions) and unrecognised wrapper forms are not
// unwrapped, so their token list is the raw argv and they never match
// an allowlist rule.
func NormaliseExec(req ExecRequest) []string {
	if tokens, ok := unwrapShellExec(req); ok {
		return tokens
	}
	return append([]string{req.Command}, req.Args...)
}

// shellProfile describes one shell's script-wrapper form: how to
// recognise it in argv, how to recover the script text, and how to
// prove the script is a single simple command. Returning tokens from
// parse is a claim that running the wrapped invocation executes
// exactly that program with those arguments; a wrapper form or script
// that cannot be proven stays unwrapped and needs approval.
type shellProfile struct {
	names []string
	// foldName matches the program base name case-insensitively, as
	// Windows binaries do. The POSIX shells keep their exact names: a
	// differently-cased name is a different binary there.
	foldName bool
	// scriptArg recovers the script text from the wrapper's argv, or
	// reports false when the argument shape is not exactly the
	// supported form (combined or extra switches, extra argv).
	scriptArg func(args []string) (string, bool)
	// parse proves the script is one simple command, or reports false
	// so the caller keeps the raw argv.
	parse func(script string) ([]string, bool)
}

var shellProfiles = []shellProfile{
	{ // sh -c <script> [name [args...]]
		names: []string{"sh", "bash", "zsh", "dash", "ash", "ksh", "fish"},
		scriptArg: func(args []string) (string, bool) {
			// Only a bare "-c" is a script argument. Combined flags
			// such as "-lc" (login shell) or "-ce" change semantics:
			// a login shell reads startup files, so unwrapping it to
			// the script body could auto-approve code the shell runs
			// before the script. Combined forms therefore stay raw
			// and never match the allowlist.
			if len(args) >= 2 && args[0] == "-c" {
				return args[1], true
			}
			return "", false
		},
		parse: tokenizeShellScript,
	},
	{ // cmd /c <script>
		names:    []string{"cmd", "cmd.exe"},
		foldName: true,
		scriptArg: func(args []string) (string, bool) {
			// cmd switches are case-insensitive, but only the bare
			// "/c" form is claimed here: "/k" keeps the prompt open,
			// "/s" rewrites quoting and "/v:on" enables a second
			// expansion pass, so none of them can be proven against
			// the script text. cmd /c also consumes the whole rest of
			// the command line, and re-joining several argv elements
			// under cmd's own rules is not something this profile
			// models: the single-script form is the only provable one.
			if len(args) == 2 && strings.EqualFold(args[0], "/c") {
				return args[1], true
			}
			return "", false
		},
		parse: tokenizeCmdScript,
	},
	{ // pwsh -NoProfile -Command <script>
		names:    []string{"powershell", "powershell.exe", "pwsh", "pwsh.exe"},
		foldName: true,
		scriptArg: func(args []string) (string, bool) {
			// Only the exact, unabbreviated pair is claimed. Without
			// -NoProfile the host runs user profiles before the
			// script (the same reasoning that keeps "-lc" raw), and
			// PowerShell accepts any unambiguous parameter prefix
			// ("-c", "-nop", ...) plus -EncodedCommand, none of which
			// this profile models. Anything else stays raw and needs
			// approval.
			if len(args) == 3 &&
				strings.EqualFold(args[0], "-NoProfile") &&
				strings.EqualFold(args[1], "-Command") {
				return args[2], true
			}
			return "", false
		},
		parse: tokenizePowerShellScript,
	},
}

func unwrapShellExec(req ExecRequest) ([]string, bool) {
	if req.Command == "" || len(req.Args) < 2 {
		return nil, false
	}
	base := filepath.Base(req.Command)
	for _, p := range shellProfiles {
		if !p.matchName(base) {
			continue
		}
		if script, ok := p.scriptArg(req.Args); ok {
			return p.parse(script)
		}
	}
	return nil, false
}

func (p shellProfile) matchName(base string) bool {
	for _, name := range p.names {
		if name == base || (p.foldName && strings.EqualFold(name, base)) {
			return true
		}
	}
	return false
}

// tokenizeShellScript splits a sh -c script into argument tokens and
// reports whether it is a simple invocation safe for allowlist
// matching. Scripts containing unquoted shell control characters
// (command separators, pipes, redirects, substitutions, subshells,
// newlines) or unterminated quotes are reported as unsafe, and the
// caller then treats the call as needing approval instead of matching
// it against the allowlist. Leading "NAME=value" assignments are
// skipped so "FOO=1 go run main.go" still matches "go run *", but
// assignments that can redirect execution or git configuration
// (PATH, LD_PRELOAD, GIT_*, ...) make the invocation unsafe: the
// visible command is not the program that actually runs.
func tokenizeShellScript(script string) ([]string, bool) {
	var tokens []string
	var cur strings.Builder
	inSingle, inDouble, escaping := false, false, false
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range script {
		if escaping {
			cur.WriteRune(r)
			escaping = false
			continue
		}
		if inSingle {
			if r == '\'' {
				inSingle = false
			} else {
				cur.WriteRune(r)
			}
			continue
		}
		if inDouble {
			if r == '"' {
				inDouble = false
			} else {
				cur.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\\':
			escaping = true
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case ' ', '\t':
			flush()
		case '&', ';', '|', '<', '>', '`', '$', '(', ')', '\n':
			return nil, false
		default:
			cur.WriteRune(r)
		}
	}
	if escaping || inSingle || inDouble {
		return nil, false
	}
	flush()
	for len(tokens) > 0 && isEnvAssignment(tokens[0]) {
		if unsafeEnvKey(tokens[0]) {
			return nil, false
		}
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return nil, false
	}
	return tokens, true
}

// tokenizeCmdScript splits a "cmd /c" script into argument tokens and
// reports whether it is provably a single simple command. cmd's
// grammar is not sh's (caret escapes, %VAR% and delayed !VAR!
// expansion, different quoting), so this models none of it: only
// scripts made of plain words are unwrapped. Command separators,
// redirects, expansion markers, quotes, backslashes and newlines all
// keep the invocation raw and approval-bound.
func tokenizeCmdScript(script string) ([]string, bool) {
	return tokenizePlainScript(script, isCmdWordRune)
}

// tokenizePowerShellScript is the same plain-word policy for
// "pwsh -NoProfile -Command": variables, subexpressions, operators,
// quotes, script blocks, splatting and array construction all keep the
// invocation raw.
func tokenizePowerShellScript(script string) ([]string, bool) {
	return tokenizePlainScript(script, isPowerShellWordRune)
}

// tokenizePlainScript splits script on spaces and tabs, accepting it
// only when every rune is a plain word rune for that shell. The
// character gate is the whole proof: with no quoting, escaping,
// expansion or control character present, the shell hands the words to
// the program exactly as split here, so the token list can be matched
// against allowlist rules.
func tokenizePlainScript(script string, wordRune func(rune) bool) ([]string, bool) {
	var tokens []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	for _, r := range script {
		switch {
		case r == ' ' || r == '\t':
			flush()
		case wordRune(r):
			word.WriteRune(r)
		default:
			return nil, false
		}
	}
	flush()
	if len(tokens) == 0 {
		return nil, false
	}
	return tokens, true
}

// isCmdWordRune reports whether r is an ordinary character inside a
// cmd word. Letters and digits of any script pass; the punctuation
// listed is inert in a plain cmd word. cmd's metacharacters — | & < >
// ^ % ! ( ) — plus the quoting and escape characters " ' \ @ and the
// delimiters , ; all stay outside the set on purpose.
func isCmdWordRune(r rune) bool {
	if isPlainWordRune(r) {
		return true
	}
	return strings.ContainsRune("_./:+-=*?", r)
}

// isPowerShellWordRune is the PowerShell counterpart. Backslash is an
// ordinary character in PowerShell (unlike sh, where it escapes), so
// native paths stay unwrappable. Backtick, quotes, $, ( ) { } [ ] ; |
// & @ # ~ ! , = and the redirection operators stay outside the set on
// purpose.
func isPowerShellWordRune(r rune) bool {
	if isPlainWordRune(r) {
		return true
	}
	return strings.ContainsRune("_./:+-\\*?", r)
}

// isPlainWordRune covers the letters and digits of any script, so
// non-ASCII names unwrap like ASCII ones. Neither shell treats them as
// syntax.
func isPlainWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// unsafeEnvKey reports whether a leading "NAME=value" assignment can
// change which program executes or which configuration it reads.
func unsafeEnvKey(assignment string) bool {
	name := assignment
	if eq := strings.IndexByte(name, '='); eq > 0 {
		name = name[:eq]
	}
	switch name {
	case "PATH", "BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS":
		return true
	}
	return strings.HasPrefix(name, "GIT_") ||
		strings.HasPrefix(name, "LD_") ||
		strings.HasPrefix(name, "DYLD_")
}

func isEnvAssignment(token string) bool {
	eq := strings.IndexByte(token, '=')
	if eq <= 0 {
		return false
	}
	name := token[:eq]
	for i, r := range name {
		if i == 0 {
			if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
				return false
			}
			continue
		}
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func parseRule(raw string) (rule, error) {
	tokens := strings.Fields(raw)
	if len(tokens) == 0 {
		return rule{}, errdefs.Validationf("sandbox allowlist: empty rule")
	}
	wildcard := false
	if tokens[len(tokens)-1] == "*" {
		wildcard = true
		tokens = tokens[:len(tokens)-1]
	}
	if len(tokens) == 0 {
		return rule{}, errdefs.Validationf(
			"sandbox allowlist: rule %q must name a command", raw)
	}
	for _, tok := range tokens {
		if strings.Contains(tok, "*") {
			return rule{}, errdefs.Validationf(
				"sandbox allowlist: rule %q: '*' is only allowed as the final token", raw)
		}
	}
	return rule{tokens: tokens, wildcard: wildcard}, nil
}

// String renders the rule back to its canonical config form.
func (r rule) String() string {
	raw := strings.Join(r.tokens, " ")
	if r.wildcard {
		raw += " *"
	}
	return raw
}

func matchRule(r rule, tokens []string) bool {
	if r.wildcard {
		if len(tokens) < len(r.tokens) {
			return false
		}
	} else if len(tokens) != len(r.tokens) {
		return false
	}
	for i, want := range r.tokens {
		got := tokens[i]
		if i == 0 {
			if !tokenEqual(want, got) {
				return false
			}
			continue
		}
		if want != got {
			return false
		}
	}
	return true
}

// tokenEqual matches a rule's program token against the command's
// program token literally, or against its base name when the rule names
// a bare program (no slash).
func tokenEqual(want, got string) bool {
	if want == got {
		return true
	}
	if strings.ContainsRune(want, '/') {
		return false
	}
	return want == filepath.Base(got)
}
