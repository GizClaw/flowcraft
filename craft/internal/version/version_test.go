package version

import "testing"

// TestComparePrereleases pins the ordering of prerelease identifiers.
// Comparing the raw suffixes instead puts alpha.10 below alpha.2, and
// that direction decides whether an update is newer and whether a host
// satisfies a floor.
func TestComparePrereleases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{name: "core identifiers are numeric", a: "0.9.0", b: "0.10.0", want: -1},
		{name: "a release outranks its prerelease", a: "1.0.0", b: "1.0.0-rc.1", want: 1},
		{name: "a prerelease ranks below the release", a: "1.0.0-beta", b: "1.0.0", want: -1},
		{name: "identifiers compare numerically", a: "1.0.0-alpha.2", b: "1.0.0-alpha.10", want: -1},
		{name: "a number ranks below a word", a: "1.0.0-1", b: "1.0.0-alpha", want: -1},
		{name: "words compare lexically", a: "1.0.0-alpha", b: "1.0.0-beta", want: -1},
		{name: "a shorter list sorts below", a: "1.0.0-alpha", b: "1.0.0-alpha.1", want: -1},
		{name: "a longer list sorts above", a: "1.0.0-alpha.1", b: "1.0.0-alpha", want: 1},
		{name: "equal lists are equal", a: "1.0.0-alpha.1", b: "1.0.0-alpha.1", want: 0},
		{name: "an invalid pair falls back to strings", a: "1.0.x", b: "1.0.y", want: -1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := Compare(testCase.a, testCase.b); got != testCase.want {
				t.Fatalf("Compare(%q, %q) = %d, want %d",
					testCase.a, testCase.b, got, testCase.want)
			}
			if got := Compare(testCase.b, testCase.a); got != -testCase.want {
				t.Fatalf("Compare(%q, %q) = %d, want %d",
					testCase.b, testCase.a, got, -testCase.want)
			}
		})
	}
}

// TestValid pins what a version field may spell: dotted numbers with an
// optional prerelease suffix.
func TestValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		version string
		want    bool
	}{
		{version: "0.1.0", want: true},
		{version: "1.0", want: true},
		{version: "1.0.0-rc.1", want: true},
		{version: "", want: false},
		{version: "v1.0.0", want: false},
		{version: "1.0.x", want: false},
		{version: "1..0", want: false},
	}
	for _, testCase := range cases {
		if got := Valid(testCase.version); got != testCase.want {
			t.Fatalf("Valid(%q) = %v, want %v",
				testCase.version, got, testCase.want)
		}
	}
}
