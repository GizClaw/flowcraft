package agent

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestResolveStringWholeKeepsType(t *testing.T) {
	board := NewBoard()
	board.SetVar("docs", []any{"a", "b"})
	board.SetVar("limit", float64(3))
	board.SetVar("ok", true)
	board.SetVar("meta", map[string]any{"k": "v"})

	cases := []struct {
		ref  string
		want any
	}{
		{"${board.docs}", []any{"a", "b"}},
		{"${board.limit}", float64(3)},
		{"${board.ok}", true},
		{"${board.meta}", map[string]any{"k": "v"}},
	}
	for _, c := range cases {
		got, err := board.ResolveString(c.ref)
		if err != nil {
			t.Fatalf("ResolveString(%q): %v", c.ref, err)
		}
		if !equalAny(got, c.want) {
			t.Fatalf("ResolveString(%q) = %#v, want %#v", c.ref, got, c.want)
		}
	}
}

func TestResolveStringInterpolates(t *testing.T) {
	board := NewBoard()
	board.SetVar("city", "Paris")
	board.SetVar("list", []any{"a", "b"})
	board.SetVar("n", float64(3))

	cases := []struct {
		in   string
		want string
	}{
		{"weather in ${board.city} please", "weather in Paris please"},
		{"items: ${board.list}", `items: ["a","b"]`},
		{"n=${board.n}", "n=3"},
	}
	for _, c := range cases {
		got, err := board.ResolveString(c.in)
		if err != nil {
			t.Fatalf("ResolveString(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ResolveString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveStringDotPath(t *testing.T) {
	board := NewBoard()
	board.SetVar("user", map[string]any{
		"name": "ada",
		"profile": map[string]any{
			"age": float64(36),
		},
	})

	got, err := board.ResolveString("${board.user.name}")
	if err != nil || got != "ada" {
		t.Fatalf("ResolveString(user.name) = %v, %v", got, err)
	}
	got, err = board.ResolveString("${board.user.profile.age}")
	if err != nil || got != float64(36) {
		t.Fatalf("ResolveString(user.profile.age) = %v, %v", got, err)
	}
	got, err = board.ResolveString("age ${board.user.profile.age}")
	if err != nil || got != "age 36" {
		t.Fatalf("ResolveString(embedded) = %v, %v", got, err)
	}
}

func TestResolveStringExactKeyWins(t *testing.T) {
	board := NewBoard()
	board.SetVar("user.name", "exact")
	board.SetVar("user", map[string]any{"name": "nested"})

	got, err := board.ResolveString("${board.user.name}")
	if err != nil || got != "exact" {
		t.Fatalf("ResolveString = %v, %v; exact key should win", got, err)
	}
}

func TestResolveStringNestedLookupTypes(t *testing.T) {
	type dict map[string]int
	board := NewBoard()
	board.SetVar("cfg", map[string]string{"env": "prod"})
	board.SetVar("limits", map[string]int{"retries": 3})
	board.SetVar("named", dict{"port": 8080})
	board.SetVar("ptr", &map[string]string{"mode": "fast"})
	board.SetVar("svc", struct{ Timeout int }{Timeout: 30})

	cases := []struct {
		in   string
		want any
	}{
		{"${board.cfg.env}", "prod"},
		{"env ${board.cfg.env}", "env prod"},
		{"${board.limits.retries}", 3},
		{"${board.named.port}", 8080},
		{"${board.ptr.mode}", "fast"},
		{"${board.svc.Timeout}", 30},
	}
	for _, c := range cases {
		got, err := board.ResolveString(c.in)
		if err != nil {
			t.Fatalf("ResolveString(%q): %v", c.in, err)
		}
		if !equalAny(got, c.want) {
			t.Fatalf("ResolveString(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}

	// Missing keys and unexported fields still fail hard.
	if _, err := board.ResolveString("${board.cfg.missing}"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing map key err = %v, want validation error", err)
	}
	if _, err := board.ResolveString("${board.svc.missing}"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing field err = %v, want validation error", err)
	}
	board.SetVar("hidden", struct{ secret string }{secret: "x"})
	if _, err := board.ResolveString("${board.hidden.secret}"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("unexported field err = %v, want validation error", err)
	}
}

func TestResolveStringDefaults(t *testing.T) {
	board := NewBoard()

	// Plain string default.
	got, err := board.ResolveString("${board.who:anon}")
	if err != nil || got != "anon" {
		t.Fatalf("string default = %v, %v", got, err)
	}
	// JSON-literal default keeps its type.
	got, err = board.ResolveString("${board.limit:3}")
	if err != nil || got != float64(3) {
		t.Fatalf("typed default = %v, %v", got, err)
	}
	got, err = board.ResolveString("${board.flag:false}")
	if err != nil || got != false {
		t.Fatalf("bool default = %v, %v", got, err)
	}
	// Embedded default is text.
	got, err = board.ResolveString("limit ${board.limit:10}")
	if err != nil || got != "limit 10" {
		t.Fatalf("embedded default = %v, %v", got, err)
	}
	// Colons inside the default are fine.
	got, err = board.ResolveString("${board.url:http://example.com}")
	if err != nil || got != "http://example.com" {
		t.Fatalf("colon default = %v, %v", got, err)
	}
	// Empty default.
	got, err = board.ResolveString("${board.x:}")
	if err != nil || got != "" {
		t.Fatalf("empty default = %v, %v", got, err)
	}
	// Escaped brace and backslash inside a default.
	got, err = board.ResolveString(`${board.x:a\}b}`)
	if err != nil || got != "a}b" {
		t.Fatalf("escaped brace default = %v, %v", got, err)
	}
	got, err = board.ResolveString(`${board.x:a\\b}`)
	if err != nil || got != `a\b` {
		t.Fatalf("escaped backslash default = %v, %v", got, err)
	}
	// An existing value beats the default.
	board.SetVar("limit", float64(9))
	got, err = board.ResolveString("${board.limit:3}")
	if err != nil || got != float64(9) {
		t.Fatalf("existing value should win = %v, %v", got, err)
	}
	// Dot-path default.
	got, err = board.ResolveString("${board.user.name:anon}")
	if err != nil || got != "anon" {
		t.Fatalf("dot-path default = %v, %v", got, err)
	}
}

func TestResolveStringMissingFails(t *testing.T) {
	board := NewBoard()
	board.SetVar("user", map[string]any{"name": "ada"})

	cases := []string{
		"${board.nope}",
		"x ${board.nope}",
		"${board.user.missing}",
		"${board.missing.name}",
	}
	for _, in := range cases {
		if _, err := board.ResolveString(in); err == nil || !errdefs.IsValidation(err) {
			t.Fatalf("ResolveString(%q) err = %v, want validation error", in, err)
		}
	}
}

func TestResolveStringMalformedFails(t *testing.T) {
	board := NewBoard()
	cases := []string{
		"${board.1x}",
		"${board.x",
		"${board.x:${board.y}}",
		"${board.x.:1}",
		"${board.}",
	}
	for _, in := range cases {
		if _, err := board.ResolveString(in); err == nil || !errdefs.IsValidation(err) {
			t.Fatalf("ResolveString(%q) err = %v, want validation error", in, err)
		}
	}
}

func TestResolveStringEscaped(t *testing.T) {
	board := NewBoard()
	board.SetVar("x", "Paris")

	// \${board.x} emits the literal reference, even when the var is missing.
	got, err := board.ResolveString(`\${board.x}`)
	if err != nil || got != "${board.x}" {
		t.Fatalf("escaped ref = %v, %v", got, err)
	}
	got, err = board.ResolveString(`\${board.nope}`)
	if err != nil || got != "${board.nope}" {
		t.Fatalf("escaped missing ref = %v, %v", got, err)
	}
	// Escaped refs mix with live refs.
	got, err = board.ResolveString(`literal \${board.x} value ${board.x}`)
	if err != nil || got != "literal ${board.x} value Paris" {
		t.Fatalf("mixed = %v, %v", got, err)
	}
	// An even run of backslashes leaves the reference live.
	got, err = board.ResolveString(`\\${board.x}`)
	if err != nil || got != `\\Paris` {
		t.Fatalf("even backslashes = %v, %v", got, err)
	}
}

func TestResolveValueWalk(t *testing.T) {
	board := NewBoard()
	board.SetVar("x", "Paris")
	board.SetVar("user", map[string]any{"name": "ada"})

	in := map[string]any{
		"a": "${board.x}",
		"b": []any{"${board.x}", "${board.user.name}"},
		"c": float64(3),
	}
	got, err := board.Resolve(in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	m := got.(map[string]any)
	if m["a"] != "Paris" {
		t.Fatalf("a = %v", m["a"])
	}
	list := m["b"].([]any)
	if list[0] != "Paris" || list[1] != "ada" {
		t.Fatalf("b = %v", list)
	}
}

func TestResolveRejectsKeyRefs(t *testing.T) {
	board := NewBoard()
	board.SetVar("k", "dynamic")

	if _, err := board.Resolve(map[string]any{"${board.k}": 1}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("key ref err = %v, want validation error", err)
	}
	if _, err := board.Resolve(map[string]any{`\${board.k}`: 1}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("escaped key ref err = %v, want validation error", err)
	}
}

func TestContainsAndExtractBoardRefs(t *testing.T) {
	cfg := map[string]any{
		"a": "${board.user.name}",
		"b": []any{"${board.y:def}", `\${board.skip}`},
		"c": float64(3),
	}
	if !ContainsBoardRef(cfg["a"]) || ContainsBoardRef(cfg["c"]) {
		t.Fatalf("ContainsBoardRef wrong")
	}
	if ContainsBoardRef(`\${board.skip}`) {
		t.Fatalf("escaped ref should not count as live")
	}
	refs := ExtractBoardRefs(cfg)
	if len(refs) != 2 || refs[0] != "user.name" || refs[1] != "y" {
		t.Fatalf("ExtractBoardRefs = %v", refs)
	}
}

func equalAny(a, b any) bool {
	switch av := a.(type) {
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !equalAny(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !equalAny(v, bv[k]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
