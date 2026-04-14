package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// TranscriptEntry represents a single transcript segment.
type TranscriptEntry struct {
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
	Text     string  `json:"text"`
}

// Transcript represents a YouTube video transcript.
type Transcript struct {
	VideoID     string
	Language    string
	Entries     []TranscriptEntry
	IsGenerated bool
}

// ExtractTranscript attempts to extract transcript from a YouTube video.
// Fetches the watch page once and reuses the HTML across both extraction paths.
func ExtractTranscript(ctx context.Context, videoID VideoID) (*Transcript, error) {
	htmlData, err := fetchWatchPage(ctx, videoID)
	if err != nil {
		return nil, err
	}

	if transcript, err := extractViaInnertubeFromHTML(ctx, htmlData, videoID); err == nil && transcript != nil {
		return transcript, nil
	}

	if transcript, err := extractViaCaptionTracksFromHTML(ctx, htmlData, videoID); err == nil && transcript != nil {
		return transcript, nil
	}

	return nil, fmt.Errorf("no transcript available for video %s", videoID)
}

// ExtractDescription extracts video description from ytInitialPlayerResponse.
// Reuses cached HTML if available, otherwise fetches the page.
func ExtractDescription(ctx context.Context, videoID VideoID) (string, error) {
	htmlData, err := fetchWatchPage(ctx, videoID)
	if err != nil {
		return "", err
	}
	return extractDescriptionFromHTML(htmlData)
}

func fetchWatchPage(ctx context.Context, videoID VideoID) ([]byte, error) {
	watchURL := "https://www.youtube.com/watch?v=" + string(videoID)
	req, err := http.NewRequestWithContext(ctx, "GET", watchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("youtube: HTTP %d fetching watch page", resp.StatusCode)
	}

	const maxWatchPageBytes = 5 << 20 // 5 MB
	return io.ReadAll(io.LimitReader(resp.Body, maxWatchPageBytes))
}

func extractViaInnertubeFromHTML(ctx context.Context, data []byte, videoID VideoID) (*Transcript, error) {
	ytcfg, err := extractYtcfg(data)
	if err != nil {
		return nil, err
	}

	apiKey := ytcfg["INNERTUBE_API_KEY"]
	if apiKey == "" {
		return nil, fmt.Errorf("no API key found")
	}

	innertubeContext := ytcfg["INNERTUBE_CONTEXT"]
	visitorData := ytcfg["VISITOR_DATA"]

	transcriptParams := extractTranscriptParams(data)
	if transcriptParams == "" {
		return nil, fmt.Errorf("no transcript params found")
	}

	apiURL := fmt.Sprintf("https://www.youtube.com/youtubei/v1/get_transcript?key=%s", apiKey)
	paramsJSON := fmt.Sprintf(`{
		"context": %s,
		"params": "%s",
		"visitorData": "%s"
	}`, innertubeContext, transcriptParams, visitorData)

	client := &http.Client{Timeout: 30 * time.Second}
	apiReq, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(paramsJSON))
	if err != nil {
		return nil, err
	}
	apiReq.Header.Set("Content-Type", "application/json")
	apiReq.Header.Set("User-Agent", "Mozilla/5.0")

	apiResp, err := client.Do(apiReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = apiResp.Body.Close() }()

	var respData map[string]interface{}
	if err := json.NewDecoder(apiResp.Body).Decode(&respData); err != nil {
		return nil, err
	}

	transcript, err := parseInnertubeTranscript(respData)
	if err != nil {
		return nil, err
	}

	transcript.VideoID = string(videoID)
	return transcript, nil
}

var (
	ytcfgSetRe      = regexp.MustCompile(`ytcfg\.set\s*\(\s*(\{[^}]+\})`)
	ytcfgKeyValueRe = regexp.MustCompile(`([A-Z_]+)\s*:\s*(?:"([^"]*)"|'([^']*)')`)
	xmlCaptionRe    = regexp.MustCompile(`<text[^>]*start="([^"]*)"[^>]*dur="([^"]*)"[^>]*>(.*?)</text>`)
)

func extractYtcfg(data []byte) (map[string]string, error) {
	matches := ytcfgSetRe.FindStringSubmatch(string(data))
	if len(matches) < 2 {
		return nil, fmt.Errorf("ytcfg not found")
	}
	return parseKeyValuePairs(matches[1])
}

func parseKeyValuePairs(s string) (map[string]string, error) {
	result := make(map[string]string)
	matches := ytcfgKeyValueRe.FindAllStringSubmatch(s, -1)
	for _, m := range matches {
		if len(m) >= 3 {
			key := m[1]
			value := m[2]
			if value == "" && len(m) >= 4 {
				value = m[3]
			}
			result[key] = value
		}
	}
	return result, nil
}

var transcriptParamsRe = regexp.MustCompile(`"getTranscriptEndpoint"\s*:\s*\{[^}]*"params"\s*:\s*"([^"]+)"`)

func extractTranscriptParams(data []byte) string {
	matches := transcriptParamsRe.FindStringSubmatch(string(data))
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func parseInnertubeTranscript(data map[string]interface{}) (*Transcript, error) {
	transcript := &Transcript{Entries: []TranscriptEntry{}}

	actions, ok := data["actions"].([]interface{})
	if !ok || len(actions) == 0 {
		return nil, fmt.Errorf("no actions in response")
	}

	for _, action := range actions {
		actionMap, ok := action.(map[string]interface{})
		if !ok {
			continue
		}

		updateEngagementPanelAction, ok := actionMap["updateEngagementPanelAction"].(map[string]interface{})
		if !ok {
			continue
		}

		panelContent, ok := updateEngagementPanelAction["content"].(map[string]interface{})
		if !ok {
			continue
		}

		sectionListRenderer, ok := panelContent["sectionListRenderer"].(map[string]interface{})
		if !ok {
			continue
		}

		contents, ok := sectionListRenderer["contents"].([]interface{})
		if !ok {
			continue
		}

		for _, content := range contents {
			transcriptSection, ok := content.(map[string]interface{})["transcriptRenderer"].(map[string]interface{})
			if !ok {
				continue
			}

			header, ok := transcriptSection["header"].(map[string]interface{})["transcriptHeaderRenderer"].(map[string]interface{})
			if ok {
				if title, ok := header["title"].(map[string]interface{})["simpleText"].(string); ok {
					transcript.Language = title
				}
			}

			lines, ok := transcriptSection["content"].(map[string]interface{})["transcriptSearchPanelRenderer"].(map[string]interface{})["lines"].([]interface{})
			if !ok {
				continue
			}

			for _, line := range lines {
				lineMap, ok := line.(map[string]interface{})["transcriptSegmentRenderer"].(map[string]interface{})
				if !ok {
					continue
				}

				startTime, _ := lineMap["startMs"].(string)
				duration, _ := lineMap["endMs"].(string)

				textMap, ok := lineMap["snippet"].(map[string]interface{})
				if !ok {
					continue
				}
				text, _ := textMap["simpleText"].(string)

				var start, dur float64
				_, _ = fmt.Sscanf(startTime, "%f", &start)
				_, _ = fmt.Sscanf(duration, "%f", &dur)

				transcript.Entries = append(transcript.Entries, TranscriptEntry{
					Start:    start / 1000.0,
					Duration: (dur - start) / 1000.0,
					Text:     text,
				})
			}
		}
	}

	if len(transcript.Entries) == 0 {
		return nil, fmt.Errorf("no transcript entries found")
	}

	return transcript, nil
}

func extractViaCaptionTracksFromHTML(ctx context.Context, data []byte, videoID VideoID) (*Transcript, error) {
	playerJSON := extractBalancedJSON(string(data), "ytInitialPlayerResponse")
	if playerJSON == "" {
		return nil, fmt.Errorf("player response not found")
	}

	var playerResp map[string]interface{}
	if err := json.Unmarshal([]byte(playerJSON), &playerResp); err != nil {
		return nil, err
	}

	captions, ok := playerResp["captions"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no captions in player response")
	}

	playerCaptions, ok := captions["playerCaptionsTracklistRenderer"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no caption tracklist")
	}

	captionTracks, ok := playerCaptions["captionTracks"].([]interface{})
	if !ok || len(captionTracks) == 0 {
		return nil, fmt.Errorf("no caption tracks")
	}

	tracks := sortCaptionTracks(captionTracks)

	for _, track := range tracks {
		trackMap, ok := track.(map[string]interface{})
		if !ok {
			continue
		}

		baseURL, ok := trackMap["baseUrl"].(string)
		if !ok {
			continue
		}

		langCode, _ := trackMap["languageCode"].(string)
		isAuto, _ := trackMap["kind"].(string)

		transcript, err := fetchAndParseCaption(ctx, baseURL)
		if err == nil && transcript != nil {
			transcript.VideoID = string(videoID)
			transcript.Language = langCode
			transcript.IsGenerated = (isAuto == "asr")
			return transcript, nil
		}
	}

	return nil, fmt.Errorf("failed to fetch any caption")
}

// extractBalancedJSON extracts a JSON object assigned to varName using balanced braces.
func extractBalancedJSON(html, varName string) string {
	re := regexp.MustCompile(varName + `\s*=\s*\{`)
	loc := re.FindStringIndex(html)
	if loc == nil {
		return ""
	}

	start := strings.Index(html[loc[0]:], "{") + loc[0]
	depth := 0
	inString := false
	escape := false

	for i := start; i < len(html); i++ {
		c := html[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inString {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[start : i+1]
			}
		}
	}
	return ""
}

func sortCaptionTracks(tracks []interface{}) []interface{} {
	type scoredTrack struct {
		score int
		track interface{}
	}

	var scored []scoredTrack
	for _, track := range tracks {
		trackMap, ok := track.(map[string]interface{})
		if !ok {
			continue
		}
		score := 0
		lang, _ := trackMap["languageCode"].(string)
		kind, _ := trackMap["kind"].(string)

		if strings.HasPrefix(lang, "en") {
			score += 10
		}
		if kind != "asr" {
			score += 5
		}

		scored = append(scored, scoredTrack{score: score, track: track})
	}

	for i := 0; i < len(scored)-1; i++ {
		for j := i + 1; j < len(scored); j++ {
			if scored[j].score > scored[i].score {
				scored[i], scored[j] = scored[j], scored[i]
			}
		}
	}

	result := make([]interface{}, len(scored))
	for i, s := range scored {
		result[i] = s.track
	}
	return result
}

func fetchAndParseCaption(ctx context.Context, url string) (*Transcript, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if strings.Contains(string(data), "events") {
		return parseJSON3Caption(data)
	}

	return parseXMLCaption(data)
}

func parseJSON3Caption(data []byte) (*Transcript, error) {
	type JSON3 struct {
		Events []struct {
			TStartMs    string `json:"tStartMs"`
			DDurationMs string `json:"dDurationMs"`
			Segs        []struct {
				UTF8 string `json:"utf8"`
			} `json:"segs"`
		} `json:"events"`
	}

	var json3 JSON3
	if err := json.Unmarshal(data, &json3); err != nil {
		return nil, err
	}

	transcript := &Transcript{Entries: []TranscriptEntry{}}
	for _, event := range json3.Events {
		var start, dur float64
		_, _ = fmt.Sscanf(event.TStartMs, "%f", &start)
		_, _ = fmt.Sscanf(event.DDurationMs, "%f", &dur)

		var text strings.Builder
		for _, seg := range event.Segs {
			text.WriteString(seg.UTF8)
		}

		transcript.Entries = append(transcript.Entries, TranscriptEntry{
			Start:    start / 1000.0,
			Duration: dur / 1000.0,
			Text:     text.String(),
		})
	}

	return transcript, nil
}

func parseXMLCaption(data []byte) (*Transcript, error) {
	transcript := &Transcript{Entries: []TranscriptEntry{}}

	re := xmlCaptionRe
	matches := re.FindAllStringSubmatch(string(data), -1)

	for _, m := range matches {
		if len(m) > 3 {
			var start, dur float64
			_, _ = fmt.Sscanf(m[1], "%f", &start)
			_, _ = fmt.Sscanf(m[2], "%f", &dur)

			transcript.Entries = append(transcript.Entries, TranscriptEntry{
				Start:    start,
				Duration: dur,
				Text:     m[3],
			})
		}
	}

	if len(transcript.Entries) == 0 {
		return nil, fmt.Errorf("no caption entries found in XML")
	}

	return transcript, nil
}

func extractDescriptionFromHTML(data []byte) (string, error) {
	playerJSON := extractBalancedJSON(string(data), "ytInitialPlayerResponse")
	if playerJSON == "" {
		return "", fmt.Errorf("player response not found")
	}

	var playerResp map[string]interface{}
	if err := json.Unmarshal([]byte(playerJSON), &playerResp); err != nil {
		return "", err
	}

	videoDetails, ok := playerResp["videoDetails"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("video details not found")
	}

	shortDescription, ok := videoDetails["shortDescription"].(string)
	if !ok {
		return "", fmt.Errorf("short description not found")
	}

	return shortDescription, nil
}

// FormatAsText formats transcript as plain text.
func FormatAsText(transcript *Transcript) string {
	if transcript == nil {
		return ""
	}

	var buf bytes.Buffer
	for _, entry := range transcript.Entries {
		buf.WriteString(entry.Text)
		buf.WriteString("\n")
	}
	return buf.String()
}
