package html

import (
	"strings"
	"testing"
)

func TestCollectSegmentsFromHTML(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		minLength  int
		wantMinLen int
	}{
		{
			name: "paragraphs with sufficient length",
			input: `<html><body><article>
				<p>This is a test paragraph with enough content to be included in the results for testing.</p>
				<p>Another paragraph that should also be long enough to pass the minimum length filter here.</p>
			</article></body></html>`,
			minLength:  30,
			wantMinLen: 2,
		},
		{
			name:       "short paragraphs excluded",
			input:      `<html><body><p>Short</p><p>Also short</p></body></html>`,
			minLength:  50,
			wantMinLen: 0,
		},
		{
			name:       "empty body",
			input:      `<html><body></body></html>`,
			minLength:  10,
			wantMinLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segments, err := CollectSegmentsFromHTML([]byte(tt.input), tt.minLength)
			if err != nil {
				t.Errorf("CollectSegmentsFromHTML() error = %v", err)
				return
			}
			if len(segments) < tt.wantMinLen {
				t.Errorf("got %d segments, want at least %d", len(segments), tt.wantMinLen)
			}
		})
	}
}

func TestExtractPlainText(t *testing.T) {
	input := `<html><body><p>Hello</p><p>World</p></body></html>`

	text, err := ExtractPlainText([]byte(input))
	if err != nil {
		t.Errorf("ExtractPlainText() error = %v", err)
	}
	if !strings.Contains(text, "Hello") || !strings.Contains(text, "World") {
		t.Errorf("ExtractPlainText() = %q, expected to contain Hello and World", text)
	}
}

func TestExtractArticleContentFallback(t *testing.T) {
	input := `<html><body><p>Short</p><div>Just some plain text in the body.</div></body></html>`

	content, err := ExtractArticleContent([]byte(input), 1000)
	if err != nil {
		t.Errorf("ExtractArticleContent() error = %v", err)
	}
	if content == "" {
		t.Error("Expected non-empty fallback content")
	}
}

func TestJoinSegments(t *testing.T) {
	segments := []Segment{
		{Tag: "p", Content: "First"},
		{Tag: "p", Content: "Second"},
	}

	result := JoinSegments(segments)
	if result != "First\nSecond" {
		t.Errorf("JoinSegments() = %q, want %q", result, "First\nSecond")
	}
}

func TestJoinSegmentsEmpty(t *testing.T) {
	if result := JoinSegments(nil); result != "" {
		t.Errorf("JoinSegments(nil) = %q, want empty", result)
	}
	if result := JoinSegments([]Segment{}); result != "" {
		t.Errorf("JoinSegments([]) = %q, want empty", result)
	}
}
