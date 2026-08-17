package plugin

import "testing"

func TestConstraintMatch(t *testing.T) {
	tests := []struct {
		constraint string
		version    string
		want       bool
	}{
		{">=0.4.0", "0.4.0", true},
		{">=0.4.0", "0.5.1", true},
		{">=0.4.0", "0.3.9", false},
		{"^1.0.0", "1.0.0", true},
		{"^1.0.0", "1.9.9", true},
		{"^1.0.0", "2.0.0", false},
		{"^0.2.0", "0.2.5", true},
		{"^0.2.0", "0.3.0", false},
		{"^0.0.3", "0.0.3", true},
		{"^0.0.3", "0.0.4", false},
		{"^0.0.0", "0.0.0", true},
		{"^0.0.0", "0.0.1", false},
		{"1.2.3", "1.2.3", true},
		{"1.2.3", "1.2.4", false},
		{">=1.0.0,<2.0.0", "1.5.0", true},
		{">=1.0.0,<2.0.0", "2.0.0", false},
		{"^1.2.3", "v1.2.3", true},
		{">=0.4.0", "v0.4.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.constraint+" / "+tt.version, func(t *testing.T) {
			c, err := parseConstraint(tt.constraint)
			if err != nil {
				t.Fatalf("parseConstraint(%q): %v", tt.constraint, err)
			}
			if got := c.Match(tt.version); got != tt.want {
				t.Fatalf("Match(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestConstraintInvalid(t *testing.T) {
	for _, raw := range []string{"", ">=x.y.z", "1.2", "1", "not-a-version", ">=1.2"} {
		if _, err := parseConstraint(raw); err == nil {
			t.Errorf("parseConstraint(%q) succeeded, want error", raw)
		}
	}
}

func TestNamedConstraint(t *testing.T) {
	any, err := parseNamedConstraint("acme.base")
	if err != nil {
		t.Fatalf("parseNamedConstraint(acme.base): %v", err)
	}
	if !any.matches("0.1.0") {
		t.Fatal("unconstrained entry must match any version")
	}

	caret, err := parseNamedConstraint("acme.base@^1.0.0")
	if err != nil {
		t.Fatalf("parseNamedConstraint: %v", err)
	}
	if !caret.matches("1.2.0") {
		t.Fatal("^1.0.0 must match 1.2.0")
	}
	if caret.matches("0.9.0") {
		t.Fatal("^1.0.0 must not match 0.9.0")
	}

	for _, raw := range []string{
		"@^1.0.0",
		"acme.base@bogus",
		"acme",        // not a reverse-domain name
		"Not.Valid",   // uppercase
		"acme..tools", // empty label
	} {
		if _, err := parseNamedConstraint(raw); err == nil {
			t.Errorf("parseNamedConstraint(%q) succeeded, want error", raw)
		}
	}
}
