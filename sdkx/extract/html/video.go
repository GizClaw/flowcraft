package html

import (
	"io"
	"net/url"
	"strings"

	"github.com/GizClaw/flowcraft/sdkx/extract/youtube"
	"github.com/PuerkitoBio/goquery"
)

// VideoInfo contains information about embedded videos.
type VideoInfo struct {
	Kind        string // "youtube", "direct"
	URL         string // Video URL
	IsVideoOnly bool
}

// VideoDetectionResult contains the result of video detection.
type VideoDetectionResult struct {
	VideoInfo   *VideoInfo
	IsVideoOnly bool
}

// DetectVideos detects embedded videos in HTML. baseURL is used
// to resolve relative src attributes.
func DetectVideos(r io.Reader, baseURLs ...string) (*VideoDetectionResult, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}

	baseURL := ""
	if len(baseURLs) > 0 {
		baseURL = baseURLs[0]
	}

	result := &VideoDetectionResult{}

	// 1. YouTube iframe embeds
	if youtubeURL, found := detectYouTubeEmbed(doc, baseURL); found {
		result.VideoInfo = &VideoInfo{Kind: "youtube", URL: youtubeURL}
		result.IsVideoOnly = true
		return result, nil
	}

	// 2. OpenGraph video (og:video, og:video:url, og:video:secure_url)
	if ogVideo := detectOGVideo(doc, baseURL); ogVideo != "" {
		if vid := extractYouTubeID(ogVideo); vid != "" {
			result.VideoInfo = &VideoInfo{Kind: "youtube", URL: "https://www.youtube.com/watch?v=" + vid}
		} else if isDirectVideoURL(ogVideo) {
			result.VideoInfo = &VideoInfo{Kind: "direct", URL: ogVideo}
		} else {
			result.VideoInfo = &VideoInfo{Kind: "direct", URL: ogVideo}
		}
		return result, nil
	}

	// 3. <video> tags
	if videoSrc := detectVideoTag(doc, baseURL); videoSrc != "" {
		if isDirectVideoURL(videoSrc) {
			result.VideoInfo = &VideoInfo{Kind: "direct", URL: videoSrc}
			return result, nil
		}
	}

	return result, nil
}

func resolveAbsoluteURL(candidate, baseURL string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
		return candidate
	}
	if baseURL == "" {
		return candidate
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return candidate
	}
	ref, err := url.Parse(candidate)
	if err != nil {
		return candidate
	}
	return base.ResolveReference(ref).String()
}

var videoExtensions = []string{".mp4", ".webm", ".mov", ".m4v"}

func isDirectVideoURL(u string) bool {
	lower := strings.ToLower(u)
	for _, ext := range videoExtensions {
		if strings.HasSuffix(lower, ext) || strings.Contains(lower, ext+"?") {
			return true
		}
	}
	return false
}

func detectYouTubeEmbed(doc *goquery.Document, baseURL string) (string, bool) {
	var youtubeURL string

	doc.Find(`iframe[src*="youtube.com/embed/"], iframe[src*="youtu.be/"], iframe[src*="youtube-nocookie.com"]`).Each(func(i int, s *goquery.Selection) {
		if youtubeURL != "" {
			return
		}
		src, _ := s.Attr("src")
		if src == "" {
			return
		}
		resolved := resolveAbsoluteURL(src, baseURL)
		vid := extractYouTubeID(resolved)
		if vid != "" {
			youtubeURL = "https://www.youtube.com/watch?v=" + vid
		}
	})

	return youtubeURL, youtubeURL != ""
}

func detectOGVideo(doc *goquery.Document, baseURL string) string {
	selectors := []string{
		"meta[property='og:video']",
		"meta[property='og:video:url']",
		"meta[property='og:video:secure_url']",
		"meta[name='og:video']",
		"meta[name='og:video:url']",
		"meta[name='og:video:secure_url']",
	}

	for _, sel := range selectors {
		val := doc.Find(sel).First().AttrOr("content", "")
		if val == "" {
			val = doc.Find(sel).First().AttrOr("value", "")
		}
		if val != "" {
			return resolveAbsoluteURL(val, baseURL)
		}
	}
	return ""
}

func extractYouTubeID(rawURL string) string {
	vid, ok := youtube.DetectVideoID(rawURL)
	if !ok {
		return ""
	}
	return string(vid)
}

func detectVideoTag(doc *goquery.Document, baseURL string) string {
	var videoSrc string

	doc.Find("video").Each(func(i int, s *goquery.Selection) {
		if videoSrc != "" {
			return
		}
		if src, exists := s.Attr("src"); exists && src != "" {
			videoSrc = resolveAbsoluteURL(src, baseURL)
			return
		}
		s.Find("source").Each(func(j int, source *goquery.Selection) {
			if videoSrc != "" {
				return
			}
			if src, exists := source.Attr("src"); exists && src != "" {
				videoSrc = resolveAbsoluteURL(src, baseURL)
			}
		})
	})

	return videoSrc
}

// IsDirectMediaURL checks if the URL is a direct media file URL.
func IsDirectMediaURL(rawURL string) bool {
	mediaExtensions := []string{
		".mp4", ".webm", ".mkv", ".avi", ".mov", ".m4v",
		".mp3", ".m4a", ".wav", ".ogg", ".flac", ".aac", ".opus",
		".jpg", ".jpeg", ".png", ".gif", ".webp",
		".pdf", ".doc", ".docx",
	}
	urlLower := strings.ToLower(rawURL)
	for _, ext := range mediaExtensions {
		if strings.HasSuffix(urlLower, ext) {
			return true
		}
	}
	return false
}
