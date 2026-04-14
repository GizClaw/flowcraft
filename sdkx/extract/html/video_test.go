package html

import (
	"strings"
	"testing"
)

func TestDetectVideos(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantVideo bool
	}{
		{
			name:      "youtube iframe",
			input:     `<html><body><iframe src="https://www.youtube.com/embed/abc123defgh"></iframe></body></html>`,
			wantVideo: true,
		},
		{
			name:      "og video youtube",
			input:     `<html><head><meta property="og:video" content="https://www.youtube.com/watch?v=abc123defgh"></head><body></body></html>`,
			wantVideo: true,
		},
		{
			name:      "html5 video",
			input:     `<html><body><video src="video.mp4"></video></body></html>`,
			wantVideo: true,
		},
		{
			name:      "no video",
			input:     `<html><body><p>Text</p></body></html>`,
			wantVideo: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := DetectVideos(strings.NewReader(tt.input))
			if err != nil {
				t.Errorf("DetectVideos() error = %v", err)
				return
			}
			if tt.wantVideo && result.VideoInfo == nil {
				t.Error("Expected video but got nil")
			}
			if !tt.wantVideo && result.VideoInfo != nil {
				t.Error("Expected no video but got one")
			}
		})
	}
}

func TestDetectVideosWithBaseURL(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		baseURL   string
		wantVideo bool
		wantURL   string
	}{
		{
			name:      "relative video src resolved with baseURL",
			input:     `<html><body><video src="/media/clip.mp4"></video></body></html>`,
			baseURL:   "https://example.com/page",
			wantVideo: true,
			wantURL:   "https://example.com/media/clip.mp4",
		},
		{
			name:      "relative video source tag resolved",
			input:     `<html><body><video><source src="assets/video.webm"></video></body></html>`,
			baseURL:   "https://example.com/blog/post",
			wantVideo: true,
			wantURL:   "https://example.com/blog/assets/video.webm",
		},
		{
			name:      "absolute src ignores baseURL",
			input:     `<html><body><video src="https://cdn.example.com/v.mp4"></video></body></html>`,
			baseURL:   "https://other.com",
			wantVideo: true,
			wantURL:   "https://cdn.example.com/v.mp4",
		},
		{
			name:      "relative src without baseURL stays relative",
			input:     `<html><body><video src="/v.mp4"></video></body></html>`,
			baseURL:   "",
			wantVideo: true,
			wantURL:   "/v.mp4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := DetectVideos(strings.NewReader(tt.input), tt.baseURL)
			if err != nil {
				t.Fatalf("DetectVideos() error = %v", err)
			}
			if tt.wantVideo && result.VideoInfo == nil {
				t.Fatal("Expected video but got nil")
			}
			if !tt.wantVideo && result.VideoInfo != nil {
				t.Fatal("Expected no video but got one")
			}
			if tt.wantVideo && result.VideoInfo.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", result.VideoInfo.URL, tt.wantURL)
			}
		})
	}
}

func TestIsDirectMediaURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://example.com/video.mp4", true},
		{"https://example.com/audio.mp3", true},
		{"https://example.com/image.jpg", true},
		{"https://example.com/image.png", true},
		{"https://example.com/document.pdf", true},
		{"https://example.com/page.html", false},
		{"https://example.com/article", false},
		{"https://example.com/path", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := IsDirectMediaURL(tt.url); got != tt.want {
				t.Errorf("IsDirectMediaURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}
