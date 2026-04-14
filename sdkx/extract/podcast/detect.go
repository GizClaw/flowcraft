package podcast

import (
	"regexp"
	"strings"
)

// PodcastType represents the type of podcast URL.
type PodcastType string

const (
	PodcastTypeApple   PodcastType = "apple"
	PodcastTypeSpotify PodcastType = "spotify"
	PodcastTypeRSS     PodcastType = "rss"
	PodcastTypeGoogle  PodcastType = "google"
	PodcastTypeGeneric PodcastType = "generic"
)

// podcastDomains is the extended list of known podcast hosting domains
// (aligned with summarize link-content-type.ts).
var podcastDomains = []string{
	"anchor.fm",
	"overcast.fm",
	"libsyn.com",
	"buzzsprout.com",
	"simplecast.com",
	"transistor.fm",
	"podbean.com",
	"spreaker.com",
	"pocketcasts.com",
	"castro.fm",
	"castbox.fm",
	"podcastaddict.com",
	"stitcher.com",
	"iheartradio.com",
	"tunein.com",
	"iheart.com",
	"pandora.com",
	"deezer.com",
	"player.fm",
	"podchaser.com",
	"megaphone.fm",
	"acast.com",
	"omnystudio.com",
	"fireside.fm",
	"audioboom.com",
	"soundon.fm",
	"podigee.com",
	"redcircle.com",
	"whooshkaa.com",
	"radiopublic.com",
}

// DetectPodcastType detects the type of podcast URL.
func DetectPodcastType(url string) PodcastType {
	lower := strings.ToLower(url)

	if strings.Contains(lower, "podcasts.apple.com") {
		return PodcastTypeApple
	}

	if strings.Contains(lower, "spotify.com") && (strings.Contains(lower, "/episode/") || strings.Contains(lower, "/show/")) {
		return PodcastTypeSpotify
	}

	if strings.Contains(lower, "podcast.google.com") {
		return PodcastTypeGoogle
	}

	if strings.HasSuffix(lower, ".rss") || strings.Contains(lower, "/feed/") ||
		strings.HasSuffix(lower, "/feed") || strings.Contains(lower, "/rss") {
		for _, domain := range podcastDomains {
			if strings.Contains(lower, domain) {
				return PodcastTypeRSS
			}
		}
		if strings.HasSuffix(lower, ".rss") || strings.HasSuffix(lower, ".xml") ||
			strings.Contains(lower, "rss") {
			return PodcastTypeRSS
		}
	}

	for _, domain := range podcastDomains {
		if strings.Contains(lower, domain) {
			return PodcastTypeGeneric
		}
	}

	return ""
}

// IsPodcastURL checks if the URL is a podcast URL.
func IsPodcastURL(url string) bool {
	return DetectPodcastType(url) != ""
}

// ExtractRSSURL extracts RSS feed URL from various podcast URLs.
func ExtractRSSURL(url string) (string, PodcastType) {
	podType := DetectPodcastType(url)

	switch podType {
	case PodcastTypeApple:
		return url, PodcastTypeApple
	case PodcastTypeSpotify:
		return url, PodcastTypeSpotify
	case PodcastTypeGoogle:
		return url, PodcastTypeGoogle
	case PodcastTypeRSS:
		return url, PodcastTypeRSS
	default:
		if strings.HasSuffix(strings.ToLower(url), ".xml") {
			return url, PodcastTypeRSS
		}
		return url, PodcastTypeGeneric
	}
}

var episodeIDRe = regexp.MustCompile(`/episode/([a-zA-Z0-9_-]+)`)

// ExtractEpisodeID extracts episode ID from podcast URLs.
func ExtractEpisodeID(url string) (string, PodcastType) {
	podType := DetectPodcastType(url)

	if matches := episodeIDRe.FindStringSubmatch(url); len(matches) > 1 {
		return matches[1], podType
	}

	return "", podType
}
