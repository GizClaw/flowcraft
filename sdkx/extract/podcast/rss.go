package podcast

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Episode represents a podcast episode.
type Episode struct {
	Title       string
	Description string
	PubDate     string
	AudioURL    string
	Duration    string
	Transcript  string
	Language    string
}

// Parser parses podcast RSS feeds.
type Parser struct {
	client *http.Client
}

// NewParser creates a new podcast parser.
func NewParser() *Parser {
	return &Parser{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

type rssRoot struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Description string    `xml:"description"`
	Items       []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string       `xml:"title"`
	Description string       `xml:"description"`
	PubDate     string       `xml:"pubDate"`
	Enclosure   rssEnclosure `xml:"enclosure"`

	// Standard <itunes:duration> with itunes namespace
	ITunesDuration string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
	// Fallback plain <duration>
	Duration string `xml:"duration"`

	// Podcasting 2.0 <podcast:transcript> with namespace
	PodcastTranscript podcastTranscript `xml:"https://podcastindex.org/namespace/1.0 transcript"`
	// Fallback plain <transcript>
	Transcript podcastTranscript `xml:"transcript"`
}

type podcastTranscript struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

type rssEnclosure struct {
	URL    string `xml:"url,attr"`
	Type   string `xml:"type,attr"`
	Length string `xml:"length,attr"`
}

func (item *rssItem) effectiveDuration() string {
	if item.ITunesDuration != "" {
		return item.ITunesDuration
	}
	return item.Duration
}

func (item *rssItem) effectiveTranscriptURL() string {
	if item.PodcastTranscript.URL != "" {
		return item.PodcastTranscript.URL
	}
	return item.Transcript.URL
}

// ParseFeed parses a podcast RSS feed.
func (p *Parser) ParseFeed(ctx context.Context, feedURL string) ([]Episode, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", feedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("podcast: HTTP %d fetching feed", resp.StatusCode)
	}

	const maxFeedBytes = 5 << 20 // 5 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return nil, err
	}

	var rss rssRoot
	if err := xml.Unmarshal(data, &rss); err != nil {
		return nil, fmt.Errorf("failed to parse RSS: %w", err)
	}

	episodes := make([]Episode, 0, len(rss.Channel.Items))
	for _, item := range rss.Channel.Items {
		ep := Episode{
			Title:       item.Title,
			Description: item.Description,
			PubDate:     item.PubDate,
			AudioURL:    item.Enclosure.URL,
			Duration:    item.effectiveDuration(),
		}

		if transcriptURL := item.effectiveTranscriptURL(); transcriptURL != "" {
			transcript, err := p.FetchTranscript(ctx, transcriptURL)
			if err == nil {
				ep.Transcript = transcript
			}
		}

		episodes = append(episodes, ep)
	}

	return episodes, nil
}

// GetLatestEpisode gets the latest episode from a feed.
func (p *Parser) GetLatestEpisode(ctx context.Context, feedURL string) (*Episode, error) {
	episodes, err := p.ParseFeed(ctx, feedURL)
	if err != nil {
		return nil, err
	}
	if len(episodes) == 0 {
		return nil, fmt.Errorf("no episodes found")
	}
	sort.Slice(episodes, func(i, j int) bool {
		ti, ei := parseRSSDate(episodes[i].PubDate)
		tj, ej := parseRSSDate(episodes[j].PubDate)
		if ei != nil || ej != nil {
			return false
		}
		return ti.After(tj)
	})
	return &episodes[0], nil
}

var rssDateFormats = []string{
	time.RFC1123Z,
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
	time.RFC3339,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700",
}

func parseRSSDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range rssDateFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unable to parse date: %s", s)
}

// FetchTranscript fetches and parses a transcript from a URL.
func (p *Parser) FetchTranscript(ctx context.Context, transcriptURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", transcriptURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("podcast: HTTP %d fetching transcript", resp.StatusCode)
	}

	const maxTranscriptBytes = 5 << 20 // 5 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTranscriptBytes))
	if err != nil {
		return "", err
	}

	if strings.Contains(transcriptURL, ".json") || (len(data) > 0 && data[0] == '{') {
		return parseJSONTranscript(data)
	}
	if strings.Contains(transcriptURL, ".vtt") || strings.Contains(string(data), "WEBVTT") {
		return parseVTTTranscript(data)
	}
	if strings.Contains(transcriptURL, ".srt") {
		return parseSRTTranscript(data)
	}

	return string(data), nil
}

func parseJSONTranscript(data []byte) (string, error) {
	type entry struct {
		Text  string `json:"text"`
		Start string `json:"start_time"`
		End   string `json:"end_time"`
	}
	type jsonTranscript struct {
		Transcript []entry `json:"transcript"`
	}

	var t jsonTranscript
	if err := json.Unmarshal(data, &t); err == nil && len(t.Transcript) > 0 {
		var sb strings.Builder
		for _, e := range t.Transcript {
			sb.WriteString(e.Text)
			sb.WriteString(" ")
		}
		return strings.TrimSpace(sb.String()), nil
	}

	type altResult struct {
		Alternatives []struct {
			Transcript string `json:"transcript"`
		} `json:"alternatives"`
	}
	type altTranscript struct {
		Results []altResult `json:"results"`
	}

	var alt altTranscript
	if err := json.Unmarshal(data, &alt); err == nil {
		var sb strings.Builder
		for _, r := range alt.Results {
			for _, a := range r.Alternatives {
				sb.WriteString(a.Transcript)
				sb.WriteString(" ")
			}
		}
		if sb.Len() > 0 {
			return strings.TrimSpace(sb.String()), nil
		}
	}

	return "", fmt.Errorf("failed to parse JSON transcript")
}

var vttTimestampRe = regexp.MustCompile(`^\d{2}:\d{2}`)

func parseVTTTranscript(data []byte) (string, error) {
	lines := strings.Split(string(data), "\n")
	var text strings.Builder

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "-->") || line == "" ||
			strings.HasPrefix(line, "WEBVTT") || strings.HasPrefix(line, "NOTE") {
			continue
		}
		if vttTimestampRe.MatchString(line) {
			continue
		}
		text.WriteString(line)
		text.WriteString(" ")
	}

	return strings.TrimSpace(text.String()), nil
}

var srtBlockRe = regexp.MustCompile(`(?s)\d+\r?\n\d{2}:\d{2}:\d{2},\d{3} --> \d{2}:\d{2}:\d{2},\d{3}\r?\n(.+?)(?:\r?\n\r?\n|\r?\n*$)`)

func parseSRTTranscript(data []byte) (string, error) {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	matches := srtBlockRe.FindAllStringSubmatch(normalized, -1)

	var text strings.Builder
	for _, m := range matches {
		if len(m) > 1 {
			text.WriteString(strings.TrimSpace(m[1]))
			text.WriteString(" ")
		}
	}

	if text.Len() == 0 {
		return string(data), nil
	}

	return strings.TrimSpace(text.String()), nil
}

// AppleLookup resolves Apple Podcasts URL to RSS feed.
func AppleLookup(ctx context.Context, podcastID string) (string, error) {
	apiURL := fmt.Sprintf("https://itunes.apple.com/lookup?id=%s&entity=podcast", podcastID)

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("podcast: HTTP %d from Apple lookup", resp.StatusCode)
	}

	var result struct {
		Results []struct {
			FeedURL string `json:"feedUrl"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Results) == 0 || result.Results[0].FeedURL == "" {
		return "", fmt.Errorf("no feed found for podcast ID %s", podcastID)
	}

	return result.Results[0].FeedURL, nil
}

var appleIDRe = regexp.MustCompile(`/id(\d+)`)

// AppleResolveURL resolves Apple Podcasts URL to RSS feed.
func AppleResolveURL(ctx context.Context, url string) (string, error) {
	matches := appleIDRe.FindStringSubmatch(url)
	if len(matches) < 2 {
		return "", fmt.Errorf("invalid Apple Podcasts URL")
	}

	podcastID := matches[1]
	return AppleLookup(ctx, podcastID)
}
