package podcast

import (
	"encoding/xml"
	"testing"
	"time"
)

func TestParseRSSDate(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		wantY   int
	}{
		{
			name:  "RFC1123Z",
			input: "Mon, 02 Jan 2006 15:04:05 -0700",
			wantY: 2006,
		},
		{
			name:  "RFC1123",
			input: "Mon, 02 Jan 2006 15:04:05 MST",
			wantY: 2006,
		},
		{
			name:  "RFC822Z",
			input: "02 Jan 06 15:04 -0700",
			wantY: 2006,
		},
		{
			name:  "RFC3339",
			input: "2024-03-15T10:30:00Z",
			wantY: 2024,
		},
		{
			name:  "single digit day",
			input: "Wed, 5 Jun 2024 08:00:00 -0500",
			wantY: 2024,
		},
		{
			name:  "with whitespace",
			input: "  Mon, 02 Jan 2006 15:04:05 -0700  ",
			wantY: 2006,
		},
		{
			name:    "invalid date",
			input:   "not-a-date",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRSSDate(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Year() != tt.wantY {
				t.Errorf("year = %d, want %d", got.Year(), tt.wantY)
			}
		})
	}
}

func TestRSSXMLParsing(t *testing.T) {
	feed := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"
	xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"
	xmlns:podcast="https://podcastindex.org/namespace/1.0">
<channel>
	<title>Test Podcast</title>
	<description>A test podcast</description>
	<item>
		<title>Episode 1</title>
		<description>First episode</description>
		<pubDate>Mon, 15 Jan 2024 10:00:00 -0500</pubDate>
		<enclosure url="https://example.com/ep1.mp3" type="audio/mpeg" length="1234567"/>
		<itunes:duration>01:30:00</itunes:duration>
		<podcast:transcript url="https://example.com/ep1.vtt" type="text/vtt"/>
	</item>
	<item>
		<title>Episode 2</title>
		<description>Second episode</description>
		<pubDate>Mon, 22 Jan 2024 10:00:00 -0500</pubDate>
		<enclosure url="https://example.com/ep2.mp3" type="audio/mpeg" length="2345678"/>
		<itunes:duration>45:00</itunes:duration>
	</item>
</channel>
</rss>`

	var rss rssRoot
	if err := xml.Unmarshal([]byte(feed), &rss); err != nil {
		t.Fatalf("failed to parse RSS: %v", err)
	}

	if rss.Channel.Title != "Test Podcast" {
		t.Errorf("title = %q, want %q", rss.Channel.Title, "Test Podcast")
	}
	if len(rss.Channel.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(rss.Channel.Items))
	}

	ep1 := rss.Channel.Items[0]
	if ep1.Title != "Episode 1" {
		t.Errorf("ep1 title = %q", ep1.Title)
	}
	if ep1.Enclosure.URL != "https://example.com/ep1.mp3" {
		t.Errorf("ep1 enclosure url = %q", ep1.Enclosure.URL)
	}
	if ep1.effectiveDuration() != "01:30:00" {
		t.Errorf("ep1 duration = %q, want 01:30:00", ep1.effectiveDuration())
	}
	if ep1.effectiveTranscriptURL() != "https://example.com/ep1.vtt" {
		t.Errorf("ep1 transcript url = %q", ep1.effectiveTranscriptURL())
	}

	ep2 := rss.Channel.Items[1]
	if ep2.effectiveTranscriptURL() != "" {
		t.Errorf("ep2 should have no transcript url, got %q", ep2.effectiveTranscriptURL())
	}
}

func TestRSSXMLParsing_FallbackDuration(t *testing.T) {
	feed := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
<channel>
	<title>Fallback Test</title>
	<item>
		<title>Ep</title>
		<duration>30:00</duration>
	</item>
</channel>
</rss>`

	var rss rssRoot
	if err := xml.Unmarshal([]byte(feed), &rss); err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if rss.Channel.Items[0].effectiveDuration() != "30:00" {
		t.Errorf("fallback duration = %q, want 30:00", rss.Channel.Items[0].effectiveDuration())
	}
}

func TestParseVTTTranscript(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "standard vtt",
			data: `WEBVTT

00:00:00.000 --> 00:00:02.000
Hello world

00:00:02.000 --> 00:00:04.000
This is a test`,
			want: "Hello world This is a test",
		},
		{
			name: "vtt with notes",
			data: `WEBVTT

NOTE This is a comment

00:00:00.000 --> 00:00:02.000
First line`,
			want: "First line",
		},
		{
			name: "empty vtt",
			data: `WEBVTT

`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVTTTranscript([]byte(tt.data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseSRTTranscript(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "standard srt",
			data: "1\n00:00:00,000 --> 00:00:02,000\nHello world\n\n2\n00:00:02,000 --> 00:00:04,000\nSecond line\n\n",
			want: "Hello world Second line",
		},
		{
			name: "srt with CRLF",
			data: "1\r\n00:00:00,000 --> 00:00:02,000\r\nFirst line\r\n\r\n",
			want: "First line",
		},
		{
			name: "no valid blocks returns raw",
			data: "just plain text",
			want: "just plain text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSRTTranscript([]byte(tt.data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseJSONTranscript(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    string
		wantErr bool
	}{
		{
			name: "standard format",
			data: `{"transcript":[{"text":"Hello","start_time":"0.0"},{"text":"world","start_time":"1.0"}]}`,
			want: "Hello world",
		},
		{
			name: "alternatives format",
			data: `{"results":[{"alternatives":[{"transcript":"Hello from alt"}]}]}`,
			want: "Hello from alt",
		},
		{
			name:    "invalid json",
			data:    `{not json}`,
			wantErr: true,
		},
		{
			name:    "empty transcript arrays",
			data:    `{"transcript":[],"results":[]}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJSONTranscript([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEpisodeDateSorting(t *testing.T) {
	episodes := []Episode{
		{Title: "Old", PubDate: "Mon, 01 Jan 2024 10:00:00 -0500"},
		{Title: "Newest", PubDate: "Fri, 15 Mar 2024 10:00:00 -0500"},
		{Title: "Middle", PubDate: "Thu, 15 Feb 2024 10:00:00 -0500"},
	}

	for i := 0; i < len(episodes)-1; i++ {
		for j := i + 1; j < len(episodes); j++ {
			ti, _ := parseRSSDate(episodes[i].PubDate)
			tj, _ := parseRSSDate(episodes[j].PubDate)
			if tj.After(ti) {
				episodes[i], episodes[j] = episodes[j], episodes[i]
			}
		}
	}

	if episodes[0].Title != "Newest" {
		t.Errorf("first episode = %q, want Newest", episodes[0].Title)
	}
	if episodes[1].Title != "Middle" {
		t.Errorf("second episode = %q, want Middle", episodes[1].Title)
	}
	if episodes[2].Title != "Old" {
		t.Errorf("third episode = %q, want Old", episodes[2].Title)
	}
}

func TestEpisodeDateSorting_WithUnparseable(t *testing.T) {
	episodes := []Episode{
		{Title: "Valid", PubDate: "Mon, 01 Jan 2024 10:00:00 -0500"},
		{Title: "Invalid", PubDate: "not-a-date"},
	}

	_, err := parseRSSDate(episodes[0].PubDate)
	if err != nil {
		t.Fatalf("valid date should parse: %v", err)
	}

	_, err = parseRSSDate(episodes[1].PubDate)
	if err == nil {
		t.Error("invalid date should fail to parse")
	}
}

func TestAppleIDRegex(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://podcasts.apple.com/us/podcast/test/id1234567890", "1234567890"},
		{"https://podcasts.apple.com/podcast/id42", "42"},
		{"https://example.com/no-id", ""},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			matches := appleIDRe.FindStringSubmatch(tt.url)
			got := ""
			if len(matches) >= 2 {
				got = matches[1]
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRSSDateFormats_Coverage(t *testing.T) {
	dates := []struct {
		input string
		year  int
		month time.Month
	}{
		{"Mon, 02 Jan 2006 15:04:05 -0700", 2006, time.January},
		{"Mon, 02 Jan 2006 15:04:05 MST", 2006, time.January},
		{"02 Jan 06 15:04 -0700", 2006, time.January},
		{"02 Jan 06 15:04 MST", 2006, time.January},
		{"2024-03-15T10:30:00Z", 2024, time.March},
		{"Wed, 5 Jun 2024 08:00:00 -0500", 2024, time.June},
		{"Wed, 5 Jun 2024 08:00:00 EST", 2024, time.June},
		{"5 Jun 2024 08:00:00 -0500", 2024, time.June},
	}

	for _, d := range dates {
		t.Run(d.input, func(t *testing.T) {
			parsed, err := parseRSSDate(d.input)
			if err != nil {
				t.Fatalf("failed to parse %q: %v", d.input, err)
			}
			if parsed.Year() != d.year {
				t.Errorf("year = %d, want %d", parsed.Year(), d.year)
			}
			if parsed.Month() != d.month {
				t.Errorf("month = %v, want %v", parsed.Month(), d.month)
			}
		})
	}
}
