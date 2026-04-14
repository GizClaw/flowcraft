package html

import (
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "basic text",
			input: "Hello World",
			want:  "Hello World",
		},
		{
			name:  "multiple spaces on line",
			input: "Hello    World",
			want:  "Hello World",
		},
		{
			name:  "preserves single newline",
			input: "Line1\nLine2",
			want:  "Line1\nLine2",
		},
		{
			name:  "merges 3+ newlines",
			input: "Line1\n\n\n\nLine2",
			want:  "Line1\n\nLine2",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
		{
			name:  "NBSP to space",
			input: "hello\u00A0world",
			want:  "hello world",
		},
		{
			name:  "removes BOM",
			input: "\uFEFFhello",
			want:  "hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Normalize(tt.input)
			if result != tt.want {
				t.Errorf("Normalize() = %q, want %q", result, tt.want)
			}
		})
	}
}

func TestApplyBudget(t *testing.T) {
	tests := []struct {
		name         string
		text         string
		maxChars     int
		wantTrunc    bool
		wantTotalGt0 bool
	}{
		{
			name:         "no truncation needed",
			text:         "Short text",
			maxChars:     100,
			wantTrunc:    false,
			wantTotalGt0: true,
		},
		{
			name:         "truncate at sentence",
			text:         "This is a long text. It has multiple sentences. Another one here.",
			maxChars:     20,
			wantTrunc:    true,
			wantTotalGt0: true,
		},
		{
			name:         "zero budget - no truncation",
			text:         "This is a long text",
			maxChars:     0,
			wantTrunc:    false,
			wantTotalGt0: true,
		},
		{
			name:         "negative budget - no truncation",
			text:         "This is a long text",
			maxChars:     -1,
			wantTrunc:    false,
			wantTotalGt0: true,
		},
		{
			name:         "empty text",
			text:         "",
			maxChars:     10,
			wantTrunc:    false,
			wantTotalGt0: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ApplyBudget(tt.text, tt.maxChars)
			if result.WasTruncated != tt.wantTrunc {
				t.Errorf("WasTruncated = %v, want %v", result.WasTruncated, tt.wantTrunc)
			}
			if tt.wantTotalGt0 && result.TotalCharacters <= 0 {
				t.Errorf("TotalCharacters = %d, expected > 0", result.TotalCharacters)
			}
			if tt.wantTrunc && len([]rune(result.Text)) > tt.maxChars {
				t.Errorf("truncated text len = %d, exceeds maxChars %d", len([]rune(result.Text)), tt.maxChars)
			}
		})
	}
}

func TestDecodeHtmlEntities(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"ampersand", "Tom &amp; Jerry", "Tom & Jerry"},
		{"html entities", "&lt;div&gt;&amp;&quot;test&quot;", `<div>&"test"`},
		{"unicode", "&#x27;", "'"},
		{"numeric", "&#60;script&#62;", "<script>"},
		{"no entities", "plain text", "plain text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DecodeHtmlEntities(tt.input)
			if result != tt.want {
				t.Errorf("DecodeHtmlEntities() = %q, want %q", result, tt.want)
			}
		})
	}
}

func TestCleanString(t *testing.T) {
	result := CleanString("  Hello   World  ", 100)
	if result.Text != "Hello World" {
		t.Errorf("CleanString() = %q, want %q", result.Text, "Hello World")
	}
	if result.WasTruncated {
		t.Error("WasTruncated should be false")
	}
	if result.TotalCharacters != 11 {
		t.Errorf("TotalCharacters = %d, want 11", result.TotalCharacters)
	}
}
