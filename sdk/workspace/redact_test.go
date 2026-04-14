package workspace

import "testing"

func TestRedactURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://user:pass@github.com/org/repo.git", "https://github.com"},
		{"https://github.com/org/repo.git?token=secret", "https://github.com"},
		{"http://example.com:8080/path#frag", "http://example.com:8080"},
		{"git://host.com/repo", "git://host.com"},
		{"github.com/org/repo", "github.com"},
		{"", ""},
		{"   ", ""},
		{"not-a-url", "not-a-url"}, // parsed as //not-a-url -> Host="not-a-url"
		{"/just/a/path", ""},
		{"://broken", ""},
		{"ftp://files.example.com/data", "ftp://files.example.com"},
	}
	for _, tt := range tests {
		got := redactURL(tt.input)
		if got != tt.want {
			t.Errorf("redactURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
