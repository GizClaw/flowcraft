package podcast

import (
	"testing"
)

func TestDetectPodcastType(t *testing.T) {
	tests := []struct {
		url      string
		wantType PodcastType
	}{
		{"https://podcasts.apple.com/podcast/test/id123", PodcastTypeApple},
		{"https://open.spotify.com/show/abc123", PodcastTypeSpotify},
		{"https://podcast.google.com/show/abc", PodcastTypeGoogle},
		{"https://example.com/feed.rss", PodcastTypeRSS},
		{"https://anchor.fm/something", PodcastTypeGeneric},
		{"https://overcast.fm/itunes123", PodcastTypeGeneric},
		{"https://example.com/page", ""},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := DetectPodcastType(tt.url); got != tt.wantType {
				t.Errorf("DetectPodcastType(%q) = %v, want %v", tt.url, got, tt.wantType)
			}
		})
	}
}

func TestIsPodcastURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://podcasts.apple.com/podcast/test", true},
		{"https://open.spotify.com/show/abc", true},
		{"https://example.com/feed.rss", true},
		{"https://example.com/article", false},
		{"https://example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := IsPodcastURL(tt.url); got != tt.want {
				t.Errorf("IsPodcastURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestExtractRSSURL(t *testing.T) {
	tests := []struct {
		url      string
		wantType PodcastType
	}{
		{"https://example.com/feed.rss", PodcastTypeRSS},
		{"https://podcasts.apple.com/podcast/test/id123", PodcastTypeApple},
		{"https://open.spotify.com/show/abc", PodcastTypeSpotify},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			_, got := ExtractRSSURL(tt.url)
			if got != tt.wantType {
				t.Errorf("ExtractRSSURL(%q) type = %v, want %v", tt.url, got, tt.wantType)
			}
		})
	}
}
