package extract

import (
	"net/http"
	"time"
)

// Format specifies the output format.
type Format string

const (
	FormatText     Format = "text"
	FormatMarkdown Format = "markdown"
)

type extractorConfig struct {
	httpClient        *http.Client
	timeout           time.Duration
	maxCharacters     int
	format            Format
	userAgent         string
	firecrawlEndpoint string
	firecrawlAPIKey   string
}

// Option configures an Extractor.
type Option func(*extractorConfig)

// WithTimeout sets the HTTP request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *extractorConfig) {
		c.timeout = d
	}
}

// WithMaxCharacters sets the maximum characters to extract.
// Pass 0 to disable truncation.
func WithMaxCharacters(n int) Option {
	return func(c *extractorConfig) {
		c.maxCharacters = n
	}
}

// WithFormat sets the output content format.
func WithFormat(f Format) Option {
	return func(c *extractorConfig) {
		c.format = f
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *extractorConfig) {
		cfg.httpClient = c
	}
}

// WithUserAgent sets a custom User-Agent string.
func WithUserAgent(ua string) Option {
	return func(c *extractorConfig) {
		c.userAgent = ua
	}
}

// WithFirecrawl configures Firecrawl API for fallback extraction.
func WithFirecrawl(endpoint, apiKey string) Option {
	return func(c *extractorConfig) {
		c.firecrawlEndpoint = endpoint
		c.firecrawlAPIKey = apiKey
	}
}
