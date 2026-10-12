package sandbox

import "testing"

func TestRenderCommandQuotesAmbiguousTokens(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "bare tokens stay bare",
			argv: []string{"git", "status", "--short"},
			want: "git status --short",
		},
		{
			name: "shell wrapper shows its script",
			argv: []string{"sh", "-c", "pip install requests"},
			want: "sh -c 'pip install requests'",
		},
		{
			name: "embedded quote escapes and reopens",
			argv: []string{"sh", "-c", "it's fine"},
			want: `sh -c 'it'\''s fine'`,
		},
		{
			name: "metacharacters are quoted",
			argv: []string{"find", ".", "-name", "*.go"},
			want: "find . -name '*.go'",
		},
		{
			name: "empty token",
			argv: []string{"git", "commit", "-m", ""},
			want: "git commit -m ''",
		},
		{
			name: "non-ascii",
			argv: []string{"echo", "h\u00e9llo"},
			want: "echo 'h\u00e9llo'",
		},
		{
			name: "no argv",
			argv: nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderCommand(tc.argv); got != tc.want {
				t.Fatalf("renderCommand(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}
