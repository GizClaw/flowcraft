package fsx

import "testing"

func TestPatternMatch(t *testing.T) {
	cases := []struct {
		pattern string
		rel     string
		want    bool
	}{
		{"a/**", "a/b/c.txt", true},
		{"a/**", "a", true},
		{"a/**", "b/c", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"**/*.md", "docs/deep/x.md", true},
		{"**/*.md", "x.md", true},
		{"docs/*", "docs/a.md", true},
		{"docs/*", "docs/sub/a.md", false},
	}
	for _, c := range cases {
		if got := Pattern(c.pattern).Match(c.rel); got != c.want {
			t.Errorf("Pattern(%q).Match(%q) = %v, want %v", c.pattern, c.rel, got, c.want)
		}
	}
}

func TestPolicyDirections(t *testing.T) {
	if !(Policy{}).HasFullRead() {
		t.Error("empty policy must mean full read")
	}
	if !(Policy{}).HasFullWrite() {
		t.Error("empty write list must mean full write")
	}
	if !(Policy{}).ReadOnly() {
		t.Error("empty write list is still read-only by CanWrite")
	}

	p := Policy{Read: []Pattern{"docs/**"}, Write: []Pattern{"out/**"}}
	if p.HasFullRead() {
		t.Error("explicit read list is not full read")
	}
	if !p.CanRead("docs/a.md") || p.CanRead("secrets/x") {
		t.Error("read enforcement is wrong")
	}
	if !p.CanWrite("out/a") || p.CanWrite("docs/a.md") {
		t.Error("write enforcement is wrong")
	}
}

func TestDenyReadWins(t *testing.T) {
	p := Policy{Read: []Pattern{"docs/**"}, DenyRead: []Pattern{"docs/private/**"}}
	if !p.CanRead("docs/public/a.md") {
		t.Error("public read should be allowed")
	}
	if p.CanRead("docs/private/secret") {
		t.Error("deny must win over allow")
	}
	if p.HasFullRead() {
		t.Error("a deny list means the read path is restricted")
	}
}

func TestUnrestrictedReadIsTheCheapPath(t *testing.T) {
	// This is the predicate wire.go uses to decide whether reads need
	// kernel enforcement: with full access there is nothing to enforce.
	if !(Policy{Write: []Pattern{"out/**"}}).HasFullRead() {
		t.Error("a write-only restriction must leave reads unrestricted")
	}
	if (Policy{DenyRead: []Pattern{"secrets/**"}}).HasFullRead() {
		t.Error("a deny list must disable the cheap read path")
	}
}
