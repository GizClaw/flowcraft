package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType ContentType
		want        string
	}{
		{"Article", ContentArticle, "article"},
		{"Transcript", ContentTranscript, "transcript"},
		{"Podcast", ContentPodcast, "podcast"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.contentType) != tt.want {
				t.Errorf("ContentType = %v, want %v", tt.contentType, tt.want)
			}
		})
	}
}

func TestExtractorInterface(t *testing.T) {
	var _ Extractor = &DefaultExtractor{}
}

func TestNew(t *testing.T) {
	ext := New()
	if ext == nil {
		t.Fatal("New() returned nil")
	}

	extWithOpts := New(
		WithTimeout(10*time.Second),
		WithMaxCharacters(1000),
		WithFormat(FormatMarkdown),
		WithUserAgent("test-agent"),
	)
	if extWithOpts == nil {
		t.Fatal("New with options returned nil")
	}
}

func TestOptions(t *testing.T) {
	tests := []struct {
		name string
		opt  Option
	}{
		{"WithTimeout", WithTimeout(10 * time.Second)},
		{"WithMaxCharacters", WithMaxCharacters(1000)},
		{"WithFormat", WithFormat(FormatMarkdown)},
		{"WithUserAgent", WithUserAgent("test-agent")},
		{"WithFirecrawl", WithFirecrawl("https://api.firecrawl.dev", "api-key")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			tt.opt(cfg)
		})
	}
}

func TestContext(t *testing.T) {
	ctx := context.Background()

	_, ok := ExtractorFrom(ctx)
	if ok {
		t.Error("Expected false from empty context")
	}

	ext := New()
	ctx = WithExtractor(ctx, ext)

	got, ok := ExtractorFrom(ctx)
	if !ok {
		t.Error("Expected true from context with extractor")
	}
	if got != ext {
		t.Error("ExtractorFrom returned wrong extractor")
	}
}

func TestDiagnostics(t *testing.T) {
	d := &Diagnostics{
		Strategy:         "readability",
		AttemptedSources: []string{"html", "readability"},
		FallbackUsed:     true,
	}

	if d.Strategy != "readability" {
		t.Errorf("Strategy = %v, want readability", d.Strategy)
	}
	if len(d.AttemptedSources) != 2 {
		t.Errorf("AttemptedSources len = %d, want 2", len(d.AttemptedSources))
	}
}

func TestExtractedContent(t *testing.T) {
	c := &ExtractedContent{
		URL:             "https://example.com",
		FinalURL:        "https://example.com/final",
		Title:           "Test",
		Content:         "Hello world",
		ContentType:     ContentArticle,
		TotalCharacters: 100,
		WordCount:       2,
		Truncated:       false,
		Metadata:        map[string]string{"og:image": "img.png"},
	}

	if c.URL != "https://example.com" {
		t.Errorf("URL = %v", c.URL)
	}
	if c.TotalCharacters != 100 {
		t.Errorf("TotalCharacters = %v, want 100", c.TotalCharacters)
	}
}

func TestStripLeadingTitle(t *testing.T) {
	tests := []struct {
		name    string
		content string
		title   string
		want    string
	}{
		{
			name:    "title at beginning",
			content: "My Title\nThis is the content.",
			title:   "My Title",
			want:    "This is the content.",
		},
		{
			name:    "no title match",
			content: "Some other content here.",
			title:   "My Title",
			want:    "Some other content here.",
		},
		{
			name:    "empty title",
			content: "Content",
			title:   "",
			want:    "Content",
		},
		{
			name:    "empty content",
			content: "",
			title:   "Title",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripLeadingTitle(tt.content, tt.title)
			if got != tt.want {
				t.Errorf("stripLeadingTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectBaseContent(t *testing.T) {
	tests := []struct {
		name       string
		transcript string
		article    string
		wantPrefix string
	}{
		{"empty transcript", "", "article text", "article text"},
		{"has transcript", "transcript text", "article text", "Transcript:\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectBaseContent(tt.transcript, tt.article)
			if len(tt.wantPrefix) > 0 && got[:len(tt.wantPrefix)] != tt.wantPrefix {
				t.Errorf("selectBaseContent() prefix = %q, want %q", got[:len(tt.wantPrefix)], tt.wantPrefix)
			}
		})
	}
}

func TestCountWords(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"hello world", 2},
		{"  multiple   spaces  ", 2},
		{"", 0},
		{"one", 1},
	}

	for _, tt := range tests {
		if got := countWords(tt.input); got != tt.want {
			t.Errorf("countWords(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestExtractWithFirecrawl_HTTPErrorStatus(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"Unauthorized", 401},
		{"Forbidden", 403},
		{"NotFound", 404},
		{"ServerError", 500},
		{"BadGateway", 502},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				if _, err := w.Write([]byte("error page")); err != nil {
					t.Errorf("write body: %v", err)
				}
			}))
			defer srv.Close()

			ext := New(WithFirecrawl(srv.URL, "test-key"), WithHTTPClient(srv.Client()))
			cfg := defaultConfig()
			cfg.firecrawlEndpoint = srv.URL
			cfg.firecrawlAPIKey = "test-key"
			cfg.httpClient = srv.Client()

			diag := &Diagnostics{}
			_, err := ext.extractWithFirecrawl(context.Background(), cfg, "https://example.com", time.Now(), diag)
			if err == nil {
				t.Fatal("expected error for HTTP status", tt.statusCode)
			}
			wantMsg := fmt.Sprintf("firecrawl returned HTTP %d", tt.statusCode)
			if err.Error() != wantMsg {
				t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
			}
		})
	}
}

func TestExtractWithFirecrawl_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"success":  true,
			"data":     "extracted content",
			"metadata": map[string]string{"title": "Test", "description": "desc"},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	ext := New()
	cfg := defaultConfig()
	cfg.firecrawlEndpoint = srv.URL
	cfg.firecrawlAPIKey = "test-key"
	cfg.httpClient = srv.Client()

	diag := &Diagnostics{}
	result, err := ext.extractWithFirecrawl(context.Background(), cfg, "https://example.com", time.Now(), diag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "extracted content" {
		t.Fatalf("content = %q, want %q", result.Content, "extracted content")
	}
	if diag.Strategy != "firecrawl" {
		t.Fatalf("strategy = %q, want %q", diag.Strategy, "firecrawl")
	}
}
