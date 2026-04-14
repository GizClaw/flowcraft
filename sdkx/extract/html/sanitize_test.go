package html

import (
	"strings"
	"testing"
)

func TestStripHiddenHTML(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		checkFn func(string) bool
	}{
		{
			name:    "preserves visible content",
			input:   "<html><body><p>Hello</p></body></html>",
			wantErr: false,
			checkFn: func(s string) bool { return strings.Contains(s, "Hello") },
		},
		{
			name:    "removes script tags",
			input:   "<html><body><p>Text</p><script>alert('x')</script></body></html>",
			wantErr: false,
			checkFn: func(s string) bool { return !strings.Contains(s, "alert") },
		},
		{
			name:    "removes style tags",
			input:   "<html><head><style>.x{color:red}</style></head><body><p>Text</p></body></html>",
			wantErr: false,
			checkFn: func(s string) bool { return !strings.Contains(s, "color:red") },
		},
		{
			name:    "removes display:none",
			input:   `<html><body><div style="display:none">Hidden</div><p>Visible</p></body></html>`,
			wantErr: false,
			checkFn: func(s string) bool { return !strings.Contains(s, "Hidden") && strings.Contains(s, "Visible") },
		},
		{
			name:    "removes hidden attribute",
			input:   `<html><body><div hidden>Hidden</div><p>Visible</p></body></html>`,
			wantErr: false,
			checkFn: func(s string) bool { return !strings.Contains(s, "Hidden") && strings.Contains(s, "Visible") },
		},
		{
			name:    "removes aria-hidden",
			input:   `<html><body><div aria-hidden="true">Hidden</div><p>Visible</p></body></html>`,
			wantErr: false,
			checkFn: func(s string) bool { return !strings.Contains(s, "Hidden") && strings.Contains(s, "Visible") },
		},
		{
			name:    "removes visibility:hidden",
			input:   `<html><body><div style="visibility:hidden">Hidden</div><p>Visible</p></body></html>`,
			wantErr: false,
			checkFn: func(s string) bool { return !strings.Contains(s, "Hidden") },
		},
		{
			name:    "removes opacity:0",
			input:   `<html><body><div style="opacity:0">Hidden</div><p>Visible</p></body></html>`,
			wantErr: false,
			checkFn: func(s string) bool { return !strings.Contains(s, "Hidden") },
		},
		{
			name:    "removes font-size:0",
			input:   `<html><body><div style="font-size:0">Hidden</div><p>Visible</p></body></html>`,
			wantErr: false,
			checkFn: func(s string) bool { return !strings.Contains(s, "Hidden") },
		},
		{
			name:    "removes HTML comments",
			input:   `<html><body><!-- comment --><p>Text</p></body></html>`,
			wantErr: false,
			checkFn: func(s string) bool { return !strings.Contains(s, "comment") && strings.Contains(s, "Text") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := StripHiddenHTML(strings.NewReader(tt.input), DefaultSanitizeConfig)
			if (err != nil) != tt.wantErr {
				t.Errorf("StripHiddenHTML() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.checkFn(result) {
				t.Errorf("StripHiddenHTML() check failed, result: %s", result)
			}
		})
	}
}

func TestMatchesHiddenStyle(t *testing.T) {
	tests := []struct {
		style string
		want  bool
	}{
		{"display:none", true},
		{"display: none", true},
		{"visibility: hidden", true},
		{"opacity: 0", true},
		{"font-size: 0", true},
		{"text-indent: -9999px", true},
		{"clip-path: inset(100%)", true},
		{"width:0;height:0;overflow:hidden", true},
		{"position:absolute;left:-9999px", true},
		{"color: red", false},
		{"font-size: 14px", false},
	}

	for _, tt := range tests {
		t.Run(tt.style, func(t *testing.T) {
			if got := matchesHiddenStyle(tt.style); got != tt.want {
				t.Errorf("matchesHiddenStyle(%q) = %v, want %v", tt.style, got, tt.want)
			}
		})
	}
}
