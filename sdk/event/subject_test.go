package event

import (
	"strings"
	"testing"
)

func TestSubject_Validate(t *testing.T) {
	cases := []struct {
		name    string
		s       Subject
		wantErr string
	}{
		{"plain", "graph.run.r1.start", ""},
		{"single segment", "ping", ""},
		{"empty", "", "empty"},
		{"leading dot", ".a.b", "leading or trailing dot"},
		{"trailing dot", "a.b.", "leading or trailing dot"},
		{"double dot", "a..b", "empty segment"},
		{"wildcard one", "a.*.b", "wildcard"},
		{"wildcard tail", "a.b.>", "wildcard"},
		{"too long", Subject(strings.Repeat("a", subjectMaxBytes+1)), "exceeds max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want err containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestPattern_Validate(t *testing.T) {
	cases := []struct {
		name    string
		p       Pattern
		wantErr string
	}{
		{"literal", "graph.run.r1.start", ""},
		{"single wildcard", "graph.run.*.start", ""},
		{"trail wildcard", "graph.run.>", ""},
		{"only trail wildcard", ">", ""},
		{"only single wildcard", "*", ""},
		{"empty", "", "empty"},
		{"trail not last", "a.>.b", "'>' must be the last segment"},
		{"wildcard inside segment", "a.b*c.d", "must occupy a whole segment"},
		{"trail inside segment", "a.b>c.d", "must occupy a whole segment"},
		{"empty segment", "a..b", "empty segment"},
		{"leading dot", ".a", "leading or trailing dot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want err containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestPattern_Matches(t *testing.T) {
	cases := []struct {
		name    string
		pattern Pattern
		subject Subject
		want    bool
	}{
		{"exact match", "a.b.c", "a.b.c", true},
		{"exact mismatch tail", "a.b.c", "a.b.d", false},
		{"exact mismatch length short", "a.b", "a.b.c", false},
		{"exact mismatch length long", "a.b.c", "a.b", false},
		{"single wildcard mid", "a.*.c", "a.b.c", true},
		{"single wildcard mid mismatch", "a.*.c", "a.b.d", false},
		{"single wildcard front", "*.b.c", "x.b.c", true},
		{"single wildcard back", "a.b.*", "a.b.x", true},
		{"single wildcard does not match zero segments", "a.b.*", "a.b", false},
		{"single wildcard does not match multiple segments", "a.*.c", "a.x.y.c", false},
		{"trail wildcard", "a.b.>", "a.b.c", true},
		{"trail wildcard deep", "a.b.>", "a.b.c.d.e", true},
		{"trail wildcard requires at least one trailing seg", "a.b.>", "a.b", false},
		{"only trail wildcard matches anything non-empty", ">", "a.b.c", true},
		{"only trail wildcard matches single seg", ">", "a", true},
		{"empty subject never matches", "a.b.c", "", false},
		{"empty pattern never matches", "", "a.b.c", false},
		{"case sensitive", "a.B.c", "a.b.c", false},
		{"mixed wildcards", "*.run.*.node.>", "graph.run.r1.node.n1.start", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.pattern.Matches(tc.subject)
			if got != tc.want {
				t.Fatalf("Matches(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
			}
		})
	}
}
