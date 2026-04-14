package twitter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultNitterInstances is the list of public Nitter instances.
var DefaultNitterInstances = []string{
	"nitter.net",
	"nitter.privacydev.net",
	"nitter.poast.org",
	"nitter.moomoo.io",
	"nitter.fly.dev",
}

// NitterClient is a client for fetching Twitter content via Nitter.
type NitterClient struct {
	mu        sync.Mutex
	client    *http.Client
	instances []string
}

// NewNitterClient creates a new Nitter client.
func NewNitterClient() *NitterClient {
	return &NitterClient{
		client:    &http.Client{Timeout: 30 * time.Second},
		instances: DefaultNitterInstances,
	}
}

// SetInstances sets custom Nitter instances.
func (n *NitterClient) SetInstances(instances []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.instances = instances
}

// FetchTweet fetches a tweet via Nitter, preserving the original URL path.
func (n *NitterClient) FetchTweet(ctx context.Context, originalURL, tweetID string) (string, error) {
	n.mu.Lock()
	instances := make([]string, len(n.instances))
	copy(instances, n.instances)
	n.mu.Unlock()

	originalPath := extractPath(originalURL)

	seed := hashSeed(originalURL)
	rotated := rotateHosts(instances, seed)

	for _, instance := range rotated {
		nitterURL := fmt.Sprintf("https://%s%s", instance, originalPath)
		content, err := n.fetchURL(ctx, nitterURL)
		if err != nil {
			continue
		}
		if isBlocked(content) {
			continue
		}

		tweet, err := extractTweetContent(content)
		if err != nil || tweet == "" {
			continue
		}

		return tweet, nil
	}

	return n.fetchDirectTwitter(ctx, originalPath, tweetID)
}

var (
	nitterPathRe = regexp.MustCompile(`(?:twitter\.com|x\.com)(/[a-zA-Z0-9_]+/status/\d+.*)`)
)

// extractPath extracts the path from a Twitter/X URL, preserving the full
// original path including username, /status/, tweetID, and any extras.
func extractPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		if m := nitterPathRe.FindStringSubmatch(rawURL); len(m) > 1 {
			return m[1]
		}
		return "/" // safe fallback: root path instead of full URL
	}
	return u.Path
}

func hashSeed(s string) int {
	hash := 0
	for _, c := range s {
		hash = (hash*31 + int(c)) & 0x7FFFFFFF
	}
	return hash
}

func rotateHosts(hosts []string, seed int) []string {
	if len(hosts) == 0 {
		return hosts
	}
	offset := seed % len(hosts)
	result := make([]string, len(hosts))
	copy(result, hosts[offset:])
	copy(result[len(hosts)-offset:], hosts[:offset])
	return result
}

func (n *NitterClient) fetchURL(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := n.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	const maxNitterBytes = 2 << 20 // 2 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxNitterBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func isBlocked(content string) bool {
	if len(content) < 500 {
		return true
	}

	lower := strings.ToLower(content)
	blockingPatterns := []string{
		"anubis",
		"proof-of-work",
		"hashcash",
		"jshelter",
		"captcha",
		"something went wrong",
		"try again",
		"privacy related extensions",
		"rate limit",
		"too many requests",
		"access denied",
	}
	for _, p := range blockingPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

var (
	tweetContentRe = regexp.MustCompile(`(?s)<div[^>]*class="[^"]*tweet-content[^"]*"[^>]*>(.*?)</div>`)
	tweetPRe       = regexp.MustCompile(`(?s)<p[^>]*class="[^"]*tweet[^"]*"[^>]*>(.*?)</p>`)
	mainRe         = regexp.MustCompile(`(?s)<main[^>]*>(.*?)</main>`)
	pRe            = regexp.MustCompile(`(?s)<p[^>]*>([^<]+)</p>`)
	htmlTagRe      = regexp.MustCompile(`<[^>]+>`)
)

func extractTweetContent(content string) (string, error) {
	if m := tweetContentRe.FindStringSubmatch(content); len(m) > 1 {
		return stripHTMLTags(m[1]), nil
	}

	if m := tweetPRe.FindStringSubmatch(content); len(m) > 1 {
		return stripHTMLTags(m[1]), nil
	}

	if m := mainRe.FindStringSubmatch(content); len(m) > 1 {
		pMatches := pRe.FindAllStringSubmatch(m[1], -1)
		var text strings.Builder
		for _, pm := range pMatches {
			if len(pm) > 1 && strings.TrimSpace(pm[1]) != "" {
				text.WriteString(stripHTMLTags(pm[1]))
				text.WriteString(" ")
			}
		}
		if text.Len() > 0 {
			return strings.TrimSpace(text.String()), nil
		}
	}

	return "", fmt.Errorf("could not extract tweet content")
}

func stripHTMLTags(s string) string {
	return strings.TrimSpace(htmlTagRe.ReplaceAllString(s, ""))
}

var directPathUserRe = regexp.MustCompile(`^/([a-zA-Z0-9_]+)/status/`)

func (n *NitterClient) fetchDirectTwitter(ctx context.Context, originalPath, tweetID string) (string, error) {
	username := "i"
	if m := directPathUserRe.FindStringSubmatch(originalPath); len(m) > 1 {
		username = m[1]
	}

	urls := []string{
		fmt.Sprintf("https://x.com/%s/status/%s", username, tweetID),
		fmt.Sprintf("https://twitter.com/%s/status/%s", username, tweetID),
	}

	for _, url := range urls {
		content, err := n.fetchURL(ctx, url)
		if err != nil {
			continue
		}
		if isBlocked(content) {
			continue
		}
		tweet, err := extractTweetContent(content)
		if err == nil && tweet != "" {
			return tweet, nil
		}
	}

	return "", fmt.Errorf("failed to fetch tweet via all methods")
}
