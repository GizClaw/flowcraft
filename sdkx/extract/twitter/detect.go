package twitter

import (
	"regexp"
	"strings"
)

// TweetType represents the type of Twitter URL.
type TweetType int

const (
	TweetTypeUnknown   TweetType = iota
	TweetTypeStatus              // /status/:id
	TweetTypeBroadcast           // /i/broadcasts/:id
	TweetTypeSpace               // /i/spaces/:id
)

var (
	statusRe    = regexp.MustCompile(`/(?:status|statuses)/(\d+)`)
	broadcastRe = regexp.MustCompile(`/broadcasts/([a-zA-Z0-9_-]+)`)
	spacesRe    = regexp.MustCompile(`/spaces/([a-zA-Z0-9_-]+)`)
)

// DetectTweetType detects the type of Twitter/X URL.
func DetectTweetType(url string) TweetType {
	lower := strings.ToLower(url)

	if statusRe.MatchString(url) {
		return TweetTypeStatus
	}

	if strings.Contains(lower, "/i/broadcasts/") {
		return TweetTypeBroadcast
	}

	if strings.Contains(lower, "/i/spaces/") {
		return TweetTypeSpace
	}

	return TweetTypeUnknown
}

// ExtractTweetID extracts tweet ID from Twitter/X URL.
func ExtractTweetID(url string) (string, TweetType) {
	tweetType := DetectTweetType(url)

	switch tweetType {
	case TweetTypeStatus:
		if matches := statusRe.FindStringSubmatch(url); len(matches) > 1 {
			return matches[1], tweetType
		}
	case TweetTypeBroadcast:
		if matches := broadcastRe.FindStringSubmatch(url); len(matches) > 1 {
			return matches[1], tweetType
		}
	case TweetTypeSpace:
		if matches := spacesRe.FindStringSubmatch(url); len(matches) > 1 {
			return matches[1], tweetType
		}
	}

	return "", tweetType
}

var twitterDomainRe = regexp.MustCompile(`(?i)(?:^https?://(?:www\.|mobile\.)?(?:twitter\.com|x\.com)/)`)

// IsTwitterURL checks if the URL is a Twitter/X URL.
func IsTwitterURL(url string) bool {
	return twitterDomainRe.MatchString(url)
}

// IsStatusURL checks if the URL is a Twitter status URL.
func IsStatusURL(url string) bool {
	return DetectTweetType(url) == TweetTypeStatus
}

// NormalizeURL normalizes Twitter/X URL to x.com format.
func NormalizeURL(url string) string {
	// Replace twitter.com with x.com
	url = strings.ReplaceAll(url, "twitter.com", "x.com")
	return url
}

// GetStatusAPIURL gets the API URL for a status.
func GetStatusAPIURL(tweetID string) string {
	return "https://api.twitter.com/2/tweets/" + tweetID
}
