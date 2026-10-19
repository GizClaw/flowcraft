package sandbox

import "testing"

// WantsStdin is the single derivation rule both spawn paths consult, so
// the four field combinations have to agree with the documented
// semantics: a declared input side, or bytes the spec already carries
// (Runner.Exec / WithDefaults). An empty slice is not input.
func TestWantsStdin(t *testing.T) {
	cases := []struct {
		name string
		spec SessionSpec
		want bool
	}{
		{name: "nothing declared", spec: SessionSpec{}, want: false},
		{name: "declared flag", spec: SessionSpec{Stdin: true}, want: true},
		{name: "carried bytes", spec: SessionSpec{Opts: ExecOptions{Stdin: []byte("x")}}, want: true},
		{name: "declared flag and bytes", spec: SessionSpec{Stdin: true, Opts: ExecOptions{Stdin: []byte("x")}}, want: true},
		{name: "empty slice is not input", spec: SessionSpec{Opts: ExecOptions{Stdin: []byte{}}}, want: false},
	}
	for _, tc := range cases {
		if got := tc.spec.WantsStdin(); got != tc.want {
			t.Errorf("%s: WantsStdin() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
