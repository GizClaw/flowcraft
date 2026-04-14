package youtube

import (
	"testing"
)

func TestDetectVideoID(t *testing.T) {
	tests := []struct {
		url    string
		wantID string
		wantOK bool
	}{
		{
			url:    "https://www.youtube.com/watch?v=abc123defgh",
			wantID: "abc123defgh",
			wantOK: true,
		},
		{
			url:    "https://youtu.be/abc123defgh",
			wantID: "abc123defgh",
			wantOK: true,
		},
		{
			url:    "https://www.youtube.com/embed/abc123defgh",
			wantID: "abc123defgh",
			wantOK: true,
		},
		{
			url:    "https://www.youtube.com/v/abc123defgh",
			wantID: "abc123defgh",
			wantOK: true,
		},
		{
			url:    "https://www.youtube.com/shorts/abc123defgh",
			wantID: "abc123defgh",
			wantOK: true,
		},
		{
			url:    "https://youtube-nocookie.com/embed/abc123defgh",
			wantID: "abc123defgh",
			wantOK: true,
		},
		{
			url:    "https://notyoutube.com/watch?v=abc123",
			wantID: "",
			wantOK: false,
		},
		{
			url:    "https://example.com",
			wantID: "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			vid, ok := DetectVideoID(tt.url)
			if ok != tt.wantOK {
				t.Errorf("DetectVideoID(%q) ok = %v, want %v", tt.url, ok, tt.wantOK)
				return
			}
			if string(vid) != tt.wantID {
				t.Errorf("DetectVideoID(%q) = %q, want %q", tt.url, vid, tt.wantID)
			}
		})
	}
}

func TestVideoIDIsValid(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"abc123defgh", true},
		{"abc123defghi", false}, // too long
		{"abc123def", false},    // too short
		{"", false},
		{"abc123defg!", false}, // invalid char
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			vid := VideoID(tt.id)
			if got := vid.IsValid(); got != tt.want {
				t.Errorf("VideoID(%q).IsValid() = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestIsYouTubeURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://www.youtube.com/watch?v=abc", true},
		{"https://youtu.be/abc", true},
		{"https://www.youtube.com/embed/abc", true},
		{"https://www.youtube.com/shorts/abc", true},
		{"https://youtube-nocookie.com/embed/abc", true},
		{"https://example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := IsYouTubeURL(tt.url); got != tt.want {
				t.Errorf("IsYouTubeURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestGetWatchURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://www.youtube.com/watch?v=abc123defgh", "https://www.youtube.com/watch?v=abc123defgh"},
		{"https://youtu.be/abc123defgh", "https://www.youtube.com/watch?v=abc123defgh"},
		{"https://www.youtube.com/embed/abc123defgh", "https://www.youtube.com/watch?v=abc123defgh"},
		{"notyoutube.com", "notyoutube.com"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := GetWatchURL(tt.url); got != tt.want {
				t.Errorf("GetWatchURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}
