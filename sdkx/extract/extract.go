// Package extract provides URL content extraction for Knowledge base
// and LLM Tool invocation. Pure Go implementation, zero external process dependency.
package extract

import (
	"context"
	"strings"
	"time"
)

// ContentType identifies the type of extracted content.
type ContentType string

const (
	ContentArticle    ContentType = "article"    // Web article
	ContentTranscript ContentType = "transcript" // Video/audio transcript
	ContentPodcast    ContentType = "podcast"    // Podcast episode
)

// ExtractedContent is the unified extraction result.
type ExtractedContent struct {
	URL             string            // Original URL
	FinalURL        string            // Final URL after redirects
	Title           string            // Page title
	Description     string            // Description/summary
	SiteName        string            // Site name
	Content         string            // Main content (cleaned, may be truncated)
	ContentType     ContentType       // Content type
	TotalCharacters int               // Character count before truncation
	WordCount       int               // Word count (based on truncated content)
	Truncated       bool              // Whether content was truncated
	Metadata        map[string]string // Additional metadata (og:image, etc.)
	Diagnostics     *Diagnostics      // Extraction diagnostics
}

// Diagnostics records extraction path and fallback information.
type Diagnostics struct {
	Strategy         string   // "readability" | "segments" | "youtube" | "podcast" | "nitter"
	AttemptedSources []string // Attempted extraction sources
	TranscriptSource string   // Transcript/subtitle source
	FallbackUsed     bool     // Whether fallback was triggered
	Notes            string   // Additional notes
}

// Extractor is the unified interface for content extraction.
type Extractor interface {
	Extract(ctx context.Context, url string, opts ...Option) (*ExtractedContent, error)
}

// New creates a DefaultExtractor with the given base options.
func New(opts ...Option) *DefaultExtractor {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}
	return &DefaultExtractor{config: cfg}
}

func defaultConfig() *extractorConfig {
	return &extractorConfig{
		timeout:       15 * time.Second,
		maxCharacters: 100_000,
		format:        FormatText,
		userAgent:     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
	}
}

func countWords(s string) int {
	return len(strings.Fields(s))
}
