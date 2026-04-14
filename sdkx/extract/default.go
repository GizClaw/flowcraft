package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdkx/extract/html"
	"github.com/GizClaw/flowcraft/sdkx/extract/podcast"
	"github.com/GizClaw/flowcraft/sdkx/extract/twitter"
	"github.com/GizClaw/flowcraft/sdkx/extract/youtube"
)

// Thresholds aligned with summarize original constants.ts.
const (
	MinReadabilityChars = 200
	MinContentChars     = 200
	MinDescriptionChars = 120
	RelativeThreshold   = 0.6
)

// DefaultExtractor is the main implementation of the Extractor interface.
type DefaultExtractor struct {
	config *extractorConfig
}

// Extract routes to specialized extractors based on URL type.
// Per-call opts override the base configuration.
func (e *DefaultExtractor) Extract(ctx context.Context, url string, opts ...Option) (*ExtractedContent, error) {
	cfg := *e.config
	for _, opt := range opts {
		opt(&cfg)
	}

	start := time.Now()
	diag := &Diagnostics{}

	if html.IsDirectMediaURL(url) {
		return nil, fmt.Errorf("direct media files are not supported (no Whisper backend): %s", redactURL(url))
	}

	if youtube.IsYouTubeURL(url) {
		diag.AttemptedSources = append(diag.AttemptedSources, "youtube")
		return e.extractYouTube(ctx, &cfg, url, start, diag)
	}

	if twitter.IsStatusURL(url) {
		diag.AttemptedSources = append(diag.AttemptedSources, "twitter/nitter")
		return e.extractTwitter(ctx, &cfg, url, start, diag)
	}

	if isBroadcastURL(url) {
		return nil, fmt.Errorf("twitter spaces/broadcasts are not supported (no Whisper backend)")
	}

	if podcast.IsPodcastURL(url) {
		diag.AttemptedSources = append(diag.AttemptedSources, "podcast")
		return e.extractPodcast(ctx, &cfg, url, start, diag)
	}

	diag.AttemptedSources = append(diag.AttemptedSources, "html")
	return e.extractHTML(ctx, &cfg, url, start, diag)
}

func isBroadcastURL(url string) bool {
	lower := strings.ToLower(url)
	return strings.Contains(lower, "/i/broadcasts/")
}

// -------------------------------------------------------------------
// YouTube extraction
// -------------------------------------------------------------------

func (e *DefaultExtractor) extractYouTube(ctx context.Context, cfg *extractorConfig, url string, start time.Time, diag *Diagnostics) (*ExtractedContent, error) {
	vid, ok := youtube.DetectVideoID(url)
	if !ok {
		return nil, fmt.Errorf("invalid YouTube URL: cannot extract video ID from %s", redactURL(url))
	}

	diag.Strategy = "youtube"

	transcript, err := youtube.ExtractTranscript(ctx, vid)
	if err == nil && transcript != nil && len(transcript.Entries) > 0 {
		text := youtube.FormatAsText(transcript)
		diag.TranscriptSource = "youtube/" + transcriptMethod(transcript)
		return e.finalize(cfg, url, url, text, "", "", ContentTranscript, diag, start), nil
	}
	diag.AttemptedSources = append(diag.AttemptedSources, "youtube/transcript_failed")

	desc, err := youtube.ExtractDescription(ctx, vid)
	if err == nil && desc != "" {
		diag.TranscriptSource = "youtube/description"
		diag.FallbackUsed = true
		return e.finalize(cfg, url, url, desc, "", "", ContentTranscript, diag, start), nil
	}

	return nil, fmt.Errorf("no transcript or description available for YouTube video %s", vid)
}

func transcriptMethod(t *youtube.Transcript) string {
	if t.IsGenerated {
		return "asr"
	}
	return "manual"
}

// -------------------------------------------------------------------
// Twitter extraction
// -------------------------------------------------------------------

func (e *DefaultExtractor) extractTwitter(ctx context.Context, cfg *extractorConfig, url string, start time.Time, diag *Diagnostics) (*ExtractedContent, error) {
	tweetID, _ := twitter.ExtractTweetID(url)
	if tweetID == "" {
		return nil, fmt.Errorf("cannot extract tweet ID from %s", redactURL(url))
	}

	diag.Strategy = "nitter"

	client := twitter.NewNitterClient()
	text, err := client.FetchTweet(ctx, url, tweetID)
	if err != nil {
		diag.Notes = err.Error()
		return nil, fmt.Errorf("twitter extraction failed: %w", err)
	}

	return e.finalize(cfg, url, url, text, "", "", ContentArticle, diag, start), nil
}

// -------------------------------------------------------------------
// Podcast extraction
// -------------------------------------------------------------------

func (e *DefaultExtractor) extractPodcast(ctx context.Context, cfg *extractorConfig, url string, start time.Time, diag *Diagnostics) (*ExtractedContent, error) {
	diag.Strategy = "podcast"

	rssURL, podType := podcast.ExtractRSSURL(url)

	if podType == podcast.PodcastTypeApple {
		resolved, err := podcast.AppleResolveURL(ctx, url)
		if err == nil && resolved != "" {
			rssURL = resolved
		}
	}

	if podType == podcast.PodcastTypeApple || podType == podcast.PodcastTypeRSS {
		parser := podcast.NewParser()
		ep, err := parser.GetLatestEpisode(ctx, rssURL)
		if err == nil {
			content := ep.Description
			if ep.Transcript != "" {
				content = "Transcript:\n" + ep.Transcript
				diag.TranscriptSource = "podcast/transcript"
			}
			return e.finalize(cfg, url, url, content, ep.Title, "", ContentPodcast, diag, start), nil
		}
		diag.FallbackUsed = true
		diag.Notes = fmt.Sprintf("RSS extraction failed: %v", err)
	}

	diag.AttemptedSources = append(diag.AttemptedSources, "html")
	return e.extractHTML(ctx, cfg, url, start, diag)
}

// -------------------------------------------------------------------
// HTML extraction (4-layer fallback chain)
// -------------------------------------------------------------------

func (e *DefaultExtractor) extractHTML(ctx context.Context, cfg *extractorConfig, url string, start time.Time, diag *Diagnostics) (*ExtractedContent, error) {
	fetchStart := time.Now()
	fetchResult, err := html.Fetch(ctx, cfg.httpClient, cfg.timeout, cfg.userAgent, url)
	if err != nil {
		return e.handleFetchError(ctx, cfg, url, start, diag, err)
	}

	data, err := io.ReadAll(fetchResult.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	diag.Notes = fmt.Sprintf("fetch=%v", time.Since(fetchStart).Truncate(time.Millisecond))

	// Anti-bot detection (combined conditions: ≥2 patterns AND short content)
	if blocked, pattern := html.DetectBlocking(bytes.NewReader(data)); blocked {
		diag.AttemptedSources = append(diag.AttemptedSources, "blocked:"+pattern)
		return e.handleBlocked(ctx, cfg, data, url, start, diag)
	}

	// Extract metadata with hostname fallback for SiteName
	metadata, jsonLd := html.ExtractMetadataWithURL(bytes.NewReader(data), fetchResult.FinalURL)

	// Detect embedded videos (with base URL for relative resolution)
	videoResult, _ := html.DetectVideos(bytes.NewReader(data), fetchResult.FinalURL)

	// --- Readability: compute once, reuse across layers ---
	readabilityResult, _ := html.ExtractWithReadability(data)

	// --- Layer 1: Readability HTML Segments vs Raw Segments ---
	var readabilitySegments string
	if readabilityResult != nil && readabilityResult.HTML != "" {
		segs, err := html.ExtractArticleContent([]byte(readabilityResult.HTML), 30)
		if err == nil {
			readabilitySegments = segs
		}
	}

	rawSegments, _ := html.ExtractArticleContent(data, 30)

	selectedContent := selectLayer1(readabilitySegments, rawSegments)
	selectedMethod := "segments"
	if selectedContent == readabilitySegments && readabilitySegments != "" {
		selectedMethod = "readability_segments"
	}

	// --- Layer 2: Readability text vs selected segments ---
	if readabilityResult != nil && readabilityResult.Text != "" {
		readText := readabilityResult.Text
		if len([]rune(readText)) >= MinReadabilityChars {
			if len([]rune(selectedContent)) < MinContentChars ||
				float64(len([]rune(readText))) >= float64(len([]rune(selectedContent)))*RelativeThreshold {
				selectedContent = readText
				selectedMethod = "readability"
			}
		}
	}

	// --- Layer 3: Description vs selected content ---
	description := ""
	title := ""
	siteName := ""
	if metadata != nil {
		description = metadata.Description
		title = metadata.Title
		siteName = metadata.SiteName
	}
	if readabilityResult != nil && readabilityResult.Title != "" {
		title = readabilityResult.Title
	}

	if len([]rune(description)) >= MinDescriptionChars {
		isPodcastLike := metadata != nil && html.IsPodcastLikeType(metadata.Type)
		if jsonLd != nil && html.IsPodcastLikeType(jsonLd.Type) {
			isPodcastLike = true
		}

		if isPodcastLike {
			selectedContent = description
			selectedMethod = "description"
		} else if len([]rune(selectedContent)) < MinContentChars {
			selectedContent = description
			selectedMethod = "description"
		} else if float64(len([]rune(description))) >= float64(len([]rune(selectedContent)))*RelativeThreshold {
			selectedContent = description
			selectedMethod = "description"
		}
	}

	// --- Layer 4: YouTube fallback for embedded videos ---
	if (selectedContent == "" || len([]rune(selectedContent)) < MinContentChars) &&
		videoResult != nil && videoResult.VideoInfo != nil && videoResult.VideoInfo.Kind == "youtube" {
		diag.AttemptedSources = append(diag.AttemptedSources, "embedded_youtube")
		vid, ok := youtube.DetectVideoID(videoResult.VideoInfo.URL)
		if ok {
			transcript, err := youtube.ExtractTranscript(ctx, vid)
			if err == nil && transcript != nil {
				text := youtube.FormatAsText(transcript)
				selectedContent = selectBaseContent(text, selectedContent)
				selectedMethod = "youtube_embed"
				diag.TranscriptSource = "embedded_youtube"
			}
		}
	}

	if selectedContent == "" {
		return e.shouldFallbackToFirecrawl(ctx, cfg, url, start, diag, readabilityResult, metadata)
	}

	diag.Strategy = selectedMethod
	selectedContent = stripLeadingTitle(selectedContent, title)

	result := e.finalize(cfg, url, fetchResult.FinalURL, selectedContent, title, siteName, ContentArticle, diag, start)
	result.Description = description
	if metadata != nil {
		result.Metadata = metadata.ToMap()
		result.SiteName = siteName
	}
	return result, nil
}

// shouldFallbackToFirecrawl implements the 3-step fallback decision:
// 1. Try Readability if not already attempted
// 2. Try description from metadata (reuses caller's metadata)
// 3. Call Firecrawl as last resort
func (e *DefaultExtractor) shouldFallbackToFirecrawl(ctx context.Context, cfg *extractorConfig, url string, start time.Time, diag *Diagnostics, readResult *html.ReadabilityResult, meta *html.Metadata) (*ExtractedContent, error) {
	// Step 1: Try Readability (reuse cached result)
	if readResult != nil && len([]rune(readResult.Text)) >= MinReadabilityChars {
		diag.Strategy = "readability"
		diag.Notes += "; recovered via readability fallback"
		return e.finalize(cfg, url, url, readResult.Text, readResult.Title, "", ContentArticle, diag, start), nil
	}

	// Step 2: Try metadata description as last content source
	if meta != nil && len([]rune(meta.Description)) >= MinDescriptionChars {
		diag.Strategy = "description"
		diag.FallbackUsed = true
		diag.Notes += "; recovered via description fallback"
		return e.finalize(cfg, url, url, meta.Description, meta.Title, meta.SiteName, ContentArticle, diag, start), nil
	}

	// Step 3: Firecrawl as absolute last resort
	if cfg.firecrawlAPIKey != "" {
		return e.extractWithFirecrawl(ctx, cfg, url, start, diag)
	}

	return nil, fmt.Errorf("all extraction layers failed for %s", redactURL(url))
}

func selectLayer1(readabilitySegments, rawSegments string) string {
	rLen := len([]rune(readabilitySegments))
	sLen := len([]rune(rawSegments))

	if rLen >= MinReadabilityChars &&
		(sLen < MinContentChars || float64(rLen) >= float64(sLen)*RelativeThreshold) {
		return readabilitySegments
	}
	return rawSegments
}

// -------------------------------------------------------------------
// Post-processing helpers
// -------------------------------------------------------------------

func (e *DefaultExtractor) finalize(cfg *extractorConfig, url, finalURL, content, title, siteName string, ct ContentType, diag *Diagnostics, start time.Time) *ExtractedContent {
	finalContent := content
	if cfg.format == FormatMarkdown {
		finalContent = toMarkdown(finalContent, title)
	}

	cleaned := html.CleanString(finalContent, cfg.maxCharacters)

	result := &ExtractedContent{
		URL:             url,
		FinalURL:        finalURL,
		Title:           title,
		SiteName:        siteName,
		Content:         cleaned.Text,
		ContentType:     ct,
		TotalCharacters: cleaned.TotalCharacters,
		WordCount:       countWords(cleaned.Text),
		Truncated:       cleaned.WasTruncated,
		Diagnostics:     diag,
	}
	return result
}

// toMarkdown wraps plain text content with a Markdown title header.
func toMarkdown(content, title string) string {
	if title == "" {
		return content
	}
	return "# " + title + "\n\n" + content
}

func stripLeadingTitle(content, title string) string {
	if title == "" || content == "" {
		return content
	}

	titleNorm := normalizeForComparison(title)
	contentNorm := normalizeForComparison(content)

	if len(contentNorm) <= len(titleNorm) {
		return content
	}

	prefix := contentNorm
	if len(prefix) > 300 {
		prefix = prefix[:300]
	}

	if strings.HasPrefix(prefix, titleNorm) {
		titleRuneLen := len([]rune(titleNorm))
		contentRunes := []rune(content)
		titleEnd := titleRuneLen
		for titleEnd < len(contentRunes) {
			r := contentRunes[titleEnd]
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
				r >= 0x80 {
				break
			}
			titleEnd++
		}
		if titleEnd < len(contentRunes) {
			return strings.TrimSpace(string(contentRunes[titleEnd:]))
		}
	}

	for _, sep := range []string{" - ", " | ", " — ", ": ", "\n"} {
		check := titleNorm + normalizeForComparison(sep)
		if strings.HasPrefix(prefix, check) {
			checkRuneLen := len([]rune(check))
			contentRunes := []rune(content)
			if checkRuneLen < len(contentRunes) {
				return strings.TrimSpace(string(contentRunes[checkRuneLen:]))
			}
		}
	}

	return content
}

func normalizeForComparison(s string) string {
	s = strings.ToLower(s)
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// selectBaseContent replaces article content with transcript when available.
func selectBaseContent(transcript, article string) string {
	if transcript == "" {
		return article
	}
	normalized := html.Normalize(transcript)
	if normalized == "" {
		return article
	}
	return "Transcript:\n" + normalized
}

// -------------------------------------------------------------------
// Fallback: Firecrawl + blocked handling
// -------------------------------------------------------------------

func (e *DefaultExtractor) handleBlocked(ctx context.Context, cfg *extractorConfig, data []byte, url string, start time.Time, diag *Diagnostics) (*ExtractedContent, error) {
	readResult, err := html.ExtractWithReadability(data)
	if err == nil && readResult != nil && len([]rune(readResult.Text)) >= MinReadabilityChars {
		diag.Strategy = "readability"
		diag.Notes += "; readability recovered from blocked page"
		return e.finalize(cfg, url, url, readResult.Text, readResult.Title, "", ContentArticle, diag, start), nil
	}

	if cfg.firecrawlAPIKey != "" {
		return e.extractWithFirecrawl(ctx, cfg, url, start, diag)
	}

	return nil, fmt.Errorf("page appears blocked by anti-bot protection and no Firecrawl configured")
}

func (e *DefaultExtractor) handleFetchError(ctx context.Context, cfg *extractorConfig, url string, start time.Time, diag *Diagnostics, err error) (*ExtractedContent, error) {
	if cfg.firecrawlAPIKey != "" {
		result, fcErr := e.extractWithFirecrawl(ctx, cfg, url, start, diag)
		if fcErr == nil && result != nil {
			return result, nil
		}
	}
	return nil, err
}

type firecrawlRequest struct {
	URL string `json:"url"`
}

type firecrawlResponse struct {
	Success  bool   `json:"success"`
	Data     string `json:"data"`
	Metadata struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"metadata"`
	Error string `json:"error"`
}

func (e *DefaultExtractor) extractWithFirecrawl(ctx context.Context, cfg *extractorConfig, url string, start time.Time, diag *Diagnostics) (*ExtractedContent, error) {
	diag.AttemptedSources = append(diag.AttemptedSources, "firecrawl")
	diag.FallbackUsed = true

	endpoint := cfg.firecrawlEndpoint
	if endpoint == "" {
		endpoint = "https://api.firecrawl.dev/v1/scrape"
	}

	fcReq := firecrawlRequest{URL: url}
	reqBody, err := json.Marshal(fcReq)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.firecrawlAPIKey)

	client := cfg.httpClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("firecrawl returned HTTP %d", resp.StatusCode)
	}

	var fcResp firecrawlResponse
	if err := json.NewDecoder(resp.Body).Decode(&fcResp); err != nil {
		return nil, err
	}
	if !fcResp.Success {
		return nil, fmt.Errorf("firecrawl error: %s", fcResp.Error)
	}

	diag.Strategy = "firecrawl"
	result := e.finalize(cfg, url, url, fcResp.Data, fcResp.Metadata.Title, "", ContentArticle, diag, start)
	result.Description = fcResp.Metadata.Description
	return result, nil
}
