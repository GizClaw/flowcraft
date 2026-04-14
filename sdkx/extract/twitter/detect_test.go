package twitter

import (
	"testing"
)

func TestDetectTweetType(t *testing.T) {
	tests := []struct {
		url  string
		want TweetType
	}{
		{"https://twitter.com/user/status/1234567890", TweetTypeStatus},
		{"https://x.com/user/status/1234567890", TweetTypeStatus},
		{"https://twitter.com/user/statuses/1234567890", TweetTypeStatus},
		{"https://x.com/user/i/broadcasts/abc123", TweetTypeBroadcast},
		{"https://x.com/user/i/spaces/abc123", TweetTypeSpace},
		{"https://twitter.com/user", TweetTypeUnknown},
		{"https://example.com", TweetTypeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := DetectTweetType(tt.url); got != tt.want {
				t.Errorf("DetectTweetType(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestExtractTweetID(t *testing.T) {
	tests := []struct {
		url    string
		wantID string
	}{
		{"https://twitter.com/user/status/1234567890", "1234567890"},
		{"https://x.com/user/status/1234567890", "1234567890"},
		{"https://x.com/user/i/broadcasts/abc123def", "abc123def"},
		{"https://x.com/user/i/spaces/xyz789", "xyz789"},
		{"https://twitter.com/user", ""},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			id, _ := ExtractTweetID(tt.url)
			if id != tt.wantID {
				t.Errorf("ExtractTweetID(%q) = %q, want %q", tt.url, id, tt.wantID)
			}
		})
	}
}

func TestIsTwitterURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://twitter.com/user", true},
		{"https://x.com/user", true},
		{"https://twitter.com/user/status/123", true},
		{"https://x.com/user/status/123", true},
		{"https://facebook.com/user", false},
		{"https://example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := IsTwitterURL(tt.url); got != tt.want {
				t.Errorf("IsTwitterURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestIsStatusURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://twitter.com/user/status/1234567890", true},
		{"https://x.com/user/status/1234567890", true},
		{"https://twitter.com/user", false},
		{"https://x.com/user/i/broadcasts/abc", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := IsStatusURL(tt.url); got != tt.want {
				t.Errorf("IsStatusURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://twitter.com/user", "https://x.com/user"},
		{"https://twitter.com/user/status/123", "https://x.com/user/status/123"},
		{"https://x.com/user", "https://x.com/user"},
		{"https://example.com", "https://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := NormalizeURL(tt.url); got != tt.want {
				t.Errorf("NormalizeURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestGetStatusAPIURL(t *testing.T) {
	want := "https://api.twitter.com/2/tweets/1234567890"
	if got := GetStatusAPIURL("1234567890"); got != want {
		t.Errorf("GetStatusAPIURL() = %q, want %q", got, want)
	}
}
