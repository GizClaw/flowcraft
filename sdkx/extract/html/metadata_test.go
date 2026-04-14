package html

import (
	"strings"
	"testing"
)

func TestExtractMetadata(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantTitle string
		wantDesc  string
	}{
		{
			name:      "basic title",
			input:     `<html><head><title>Test Title</title><meta name="description" content="Test Description"></head><body></body></html>`,
			wantTitle: "Test Title",
			wantDesc:  "Test Description",
		},
		{
			name:      "opengraph overrides title",
			input:     `<html><head><title>HTML Title</title><meta property="og:title" content="OG Title"><meta property="og:description" content="OG Desc"></head><body></body></html>`,
			wantTitle: "OG Title",
			wantDesc:  "OG Desc",
		},
		{
			name:      "empty HTML",
			input:     `<html><head></head><body></body></html>`,
			wantTitle: "",
			wantDesc:  "",
		},
		{
			name:      "deeply nested title",
			input:     `<html><head><title>Deep Title</title></head><body><p>Content</p></body></html>`,
			wantTitle: "Deep Title",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ExtractMetadata(strings.NewReader(tt.input))
			if err != nil {
				t.Errorf("ExtractMetadata() error = %v", err)
				return
			}
			if result == nil {
				t.Fatal("Expected non-nil result")
			}
			if tt.wantTitle != "" && result.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", result.Title, tt.wantTitle)
			}
			if tt.wantDesc != "" && result.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", result.Description, tt.wantDesc)
			}
		})
	}
}

func TestExtractMetadataJSONLD(t *testing.T) {
	input := `<html><head>
	<script type="application/ld+json">
	{"@type":"Article","description":"JSON-LD description","name":"Article Name"}
	</script>
	</head><body></body></html>`

	result, jsonLd := ExtractMetadataWithURL(strings.NewReader(input), "https://example.com/page")
	if result.Description != "JSON-LD description" {
		t.Errorf("Description = %q, want %q", result.Description, "JSON-LD description")
	}
	if jsonLd == nil {
		t.Fatal("Expected non-nil jsonLd")
	}
	if jsonLd.Type != "article" {
		t.Errorf("jsonLd.Type = %q, want %q", jsonLd.Type, "article")
	}
	if result.SiteName != "example.com" {
		t.Errorf("SiteName = %q, want %q", result.SiteName, "example.com")
	}
}

func TestMetadataToMap(t *testing.T) {
	m := &Metadata{
		Image:     "img.png",
		Author:    "Test Author",
		Published: "2024-01-01",
		Type:      "article",
	}
	mp := m.ToMap()
	if mp["og:image"] != "img.png" {
		t.Errorf("og:image = %q", mp["og:image"])
	}
	if mp["author"] != "Test Author" {
		t.Errorf("author = %q", mp["author"])
	}
}
