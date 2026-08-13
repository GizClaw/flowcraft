package sandbox

import (
	"path/filepath"
	"strings"
	"sync"

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
// "sh -c" wrappers, and scripts containing shell control characters are
// never matched, so they fall through to the approver. Like every
// predicate, the allowlist is the tripwire, not the wall — OS-level
// backend enforcement remains the security boundary.
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

// NormaliseExec returns the token list used for allowlist matching. A
// "sh -c <script>" invocation is unwrapped to the script's tokens when
// the script is a simple command; complex scripts (shell control
// characters, substitutions, redirects) are not unwrapped, so their
// token list is the raw argv and they never match an allowlist rule.
func NormaliseExec(req ExecRequest) []string {
	if tokens, ok := unwrapShellExec(req); ok {
		return tokens
	}
	return append([]string{req.Command}, req.Args...)
}

func unwrapShellExec(req ExecRequest) ([]string, bool) {
	if req.Command == "" || len(req.Args) < 2 {
		return nil, false
	}
	if !isShellName(filepath.Base(req.Command)) || !isDashCArg(req.Args[0]) {
		return nil, false
	}
	tokens, ok := tokenizeShellScript(req.Args[1])
	if !ok {
		return nil, false
	}
	return tokens, true
}

func isShellName(name string) bool {
	switch name {
	case "sh", "bash", "zsh", "dash", "ash", "ksh", "fish":
		return true
	default:
		return false
	}
}

func isDashCArg(arg string) bool {
	if arg == "-c" {
		return true
	}
	return strings.HasPrefix(arg, "-") && strings.Contains(arg, "c")
}

// tokenizeShellScript splits a sh -c script into argument tokens and
// reports whether it is a simple invocation safe for allowlist
// matching. Scripts containing unquoted shell control characters
// (command separators, pipes, redirects, substitutions, subshells,
// newlines) or unterminated quotes are reported as unsafe, and the
// caller then treats the call as needing approval instead of matching
// it against the allowlist. Leading "NAME=value" assignments are
// skipped so "FOO=1 go run main.go" still matches "go run *".
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
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return nil, false
	}
	return tokens, true
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
