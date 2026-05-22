package textsearch

import "testing"

func TestStem_BasicCases(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"running", "run"},
		{"programming", "program"},
		{"caresses", "caress"},
		{"ponies", "poni"},
		{"cats", "cat"},
		{"agreed", "agre"},
		{"disabled", "disabl"},
		{"matting", "mat"},
		{"mating", "mate"},
		{"meeting", "meet"},
		{"milling", "mill"},
		{"messing", "mess"},
		{"meetings", "meet"},
		{"", ""},
		{"a", "a"},
		{"go", "go"},
	}
	for _, tc := range cases {
		got := Stem(tc.input)
		if got != tc.want {
			t.Errorf("Stem(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestStem_Step2(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"relational", "relat"},
		{"conditional", "condit"},
		{"rational", "ration"},
		{"valenci", "valenc"},
		{"hesitanci", "hesit"},
		{"digitizer", "digit"},
		{"conformabli", "conform"},
		{"radicalli", "radic"},
		{"differentli", "differ"},
		{"vileli", "vile"},
		{"analogousli", "analog"},
		{"vietnamization", "vietnam"},
		{"predication", "predic"},
		{"operator", "oper"},
		{"feudalism", "feudal"},
		{"decisiveness", "decis"},
		{"hopefulness", "hope"},
		{"callousness", "callous"},
		{"formaliti", "formal"},
	}
	for _, tc := range cases {
		got := Stem(tc.input)
		if got != tc.want {
			t.Errorf("Stem(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestStem_Idempotent(t *testing.T) {
	words := []string{"run", "program", "cat", "go", "test"}
	for _, w := range words {
		first := Stem(w)
		second := Stem(first)
		if first != second {
			t.Errorf("Stem not idempotent for %q: Stem=%q, Stem(Stem)=%q", w, first, second)
		}
	}
}
