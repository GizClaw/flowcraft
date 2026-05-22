package domain

import "testing"

func TestWriteMode_String(t *testing.T) {
	cases := []struct {
		in   WriteMode
		want string
	}{
		{WriteModeSync, "sync"},
		{WriteModeAsyncSemantic, "async_semantic"},
		{WriteMode(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("WriteMode(%d).String() = %q, want %q", int(tc.in), got, tc.want)
		}
	}
}
