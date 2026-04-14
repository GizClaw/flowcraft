package html

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// FetchResult contains the result of an HTTP fetch operation.
type FetchResult struct {
	Body        *bytes.Reader
	StatusCode  int
	FinalURL    string
	ContentType string
}

// Fetch fetches HTML content from a URL and returns the body as an in-memory reader.
func Fetch(ctx context.Context, httpClient *http.Client, timeout time.Duration, userAgent, urlStr string) (*FetchResult, error) {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				return nil
			},
		}
	}

	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Connection", "keep-alive")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, resp.Request.URL.Host)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "" && !isAcceptableContentType(contentType) {
		return nil, fmt.Errorf("not an HTML page: Content-Type is %s", contentType)
	}

	const maxHTMLBytes = 10 << 20 // 10 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	finalURL := resp.Request.URL.String()

	return &FetchResult{
		Body:        bytes.NewReader(data),
		StatusCode:  resp.StatusCode,
		FinalURL:    finalURL,
		ContentType: contentType,
	}, nil
}

func isAcceptableContentType(ct string) bool {
	acceptable := []string{
		"text/html",
		"application/xhtml+xml",
		"application/xml",
		"text/xml",
		"application/rss+xml",
		"application/atom+xml",
	}
	lower := strings.ToLower(ct)
	for _, a := range acceptable {
		if strings.Contains(lower, a) {
			return true
		}
	}
	return strings.HasPrefix(lower, "text/")
}

// ExtractText extracts plain text from HTML using basic tokenization.
func ExtractText(r io.Reader) (string, error) {
	var buf bytes.Buffer
	tokenizer := html.NewTokenizer(r)
	inBody := false

	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return buf.String(), nil
			}
			return "", tokenizer.Err()
		case html.StartTagToken:
			token := tokenizer.Token()
			if token.DataAtom == atom.Body {
				inBody = true
			}
			if isBlockElement(token.DataAtom) {
				buf.WriteString("\n")
			}
		case html.EndTagToken:
			token := tokenizer.Token()
			if token.DataAtom == atom.Body {
				inBody = false
			}
			if isBlockElement(token.DataAtom) {
				buf.WriteString("\n")
			}
		case html.TextToken:
			if inBody {
				text := strings.TrimSpace(tokenizer.Token().Data)
				if len(text) > 0 {
					buf.WriteString(text)
					buf.WriteString(" ")
				}
			}
		}
	}
}

func isBlockElement(a atom.Atom) bool {
	switch a {
	case atom.P, atom.Div, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Br, atom.Li, atom.Tr, atom.Td, atom.Th, atom.Blockquote,
		atom.Pre, atom.Form, atom.Address, atom.Figure, atom.Figcaption,
		atom.Header, atom.Footer, atom.Nav, atom.Section, atom.Article,
		atom.Aside, atom.Main, atom.Hgroup, atom.Details, atom.Summary:
		return true
	}
	return false
}

// DetectBlocking detects anti-bot protection using combined conditions.
// Returns true only when at least 2 indicators match AND content is short
// (< MinDocCharsForBlockDetection), reducing false positives on legitimate
// pages that simply mention "captcha" or "cloudflare".
const MinDocCharsForBlockDetection = 5000

func DetectBlocking(r io.Reader) (bool, string) {
	data, err := io.ReadAll(r)
	if err != nil {
		return false, ""
	}
	content := strings.ToLower(string(data))

	if len(data) > MinDocCharsForBlockDetection {
		return false, ""
	}

	blockingPatterns := []string{
		"access denied",
		"attention required",
		"captcha",
		"cloudflare",
		"enable javascript",
		"forbidden",
		"please turn javascript on",
		"verify you are human",
		"anubis",
		"proof-of-work",
		"jshelter",
	}

	var matched []string
	for _, pattern := range blockingPatterns {
		if strings.Contains(content, pattern) {
			matched = append(matched, pattern)
		}
	}

	if len(matched) >= 2 {
		return true, strings.Join(matched, "+")
	}
	return false, ""
}
