//go:build windows

package paths

import "testing"

func TestRelFold(t *testing.T) {
	cases := []struct {
		name   string
		root   string
		target string
		want   string
		ok     bool
	}{
		{"same case", `C:\work`, `C:\work\a.txt`, `a.txt`, true},
		{"folded root", `C:\Work`, `c:\work\a.txt`, `a.txt`, true},
		{"root itself", `C:\Work`, `c:\work`, ".", true},
		{"nested", `C:\work`, `C:\work\deep\x.txt`, `deep\x.txt`, true},
		{"sibling", `C:\work`, `C:\other\a.txt`, "", false},
		{"prefix but not parent", `C:\work`, `C:\workshop\a.txt`, "", false},
		{"other volume", `C:\work`, `D:\work\a.txt`, "", false},
	}
	for _, tc := range cases {
		got, ok := RelFold(tc.root, tc.target)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s: RelFold(%q, %q) = %q, %v; want %q, %v",
				tc.name, tc.root, tc.target, got, ok, tc.want, tc.ok)
		}
	}
}

func TestWithinFoldOnWindowsSpellings(t *testing.T) {
	if !WithinFold(`C:\Work`, `c:\work\a.txt`) {
		t.Error("WithinFold should fold case")
	}
	if WithinFold(`C:\work`, `C:\workshop\a.txt`) {
		t.Error("WithinFold matched a sibling that shares a prefix")
	}
}
