package youtube

import (
	"regexp"
	"strings"
)

// VideoID represents a YouTube video ID.
type VideoID string

var validIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)

// IsValid checks if the video ID is valid (11 characters, alphanumeric + hyphen/underscore).
func (v VideoID) IsValid() bool {
	if len(v) != 11 {
		return false
	}
	return validIDRe.MatchString(string(v))
}

var videoIDPatterns = []struct {
	regex *regexp.Regexp
	idx   int
}{
	{regexp.MustCompile(`[?&]v=([a-zA-Z0-9_-]{11})`), 1},
	{regexp.MustCompile(`youtu\.be/([a-zA-Z0-9_-]{11})`), 1},
	{regexp.MustCompile(`youtube\.com/embed/([a-zA-Z0-9_-]{11})`), 1},
	{regexp.MustCompile(`youtube\.com/v/([a-zA-Z0-9_-]{11})`), 1},
	{regexp.MustCompile(`youtube\.com/shorts/([a-zA-Z0-9_-]{11})`), 1},
	{regexp.MustCompile(`youtube-nocookie\.com/embed/([a-zA-Z0-9_-]{11})`), 1},
}

// DetectVideoID extracts video ID from various YouTube URL formats.
func DetectVideoID(url string) (VideoID, bool) {
	patterns := videoIDPatterns

	for _, p := range patterns {
		matches := p.regex.FindStringSubmatch(url)
		if len(matches) > p.idx {
			vid := VideoID(matches[p.idx])
			if vid.IsValid() {
				return vid, true
			}
		}
	}

	return "", false
}

// IsYouTubeURL checks if the URL is a YouTube URL.
func IsYouTubeURL(url string) bool {
	lower := strings.ToLower(url)
	patterns := []string{
		"youtube.com/watch",
		"youtube.com/embed",
		"youtube.com/v/",
		"youtube.com/shorts",
		"youtu.be/",
		"youtube-nocookie.com",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// GetWatchURL converts any YouTube URL format to a watch URL.
func GetWatchURL(url string) string {
	vid, ok := DetectVideoID(url)
	if !ok {
		return url
	}
	return "https://www.youtube.com/watch?v=" + string(vid)
}
