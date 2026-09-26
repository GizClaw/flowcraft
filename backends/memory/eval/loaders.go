package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// LoaderOptions configures dataset conversion.
type LoaderOptions struct {
	Scope  Scope
	Budget corememory.Budget
	// Samples bounds how many conversations are converted (0 = all). The image
	// downloader only materializes images for the conversations that will run,
	// so a small smoke run does not fetch the whole benchmark.
	Samples int
	// Images selects how turns that carry an image are ingested:
	//   "" or "annotation" — fold the caption into the text (historical shape)
	//   "native"           — attach the image itself as a message part
	//   "both"             — attach the image and keep the caption text
	Images string
}

// LoaderStats summarizes one conversion.
type LoaderStats struct {
	Conversations int `json:"conversations"`
	Turns         int `json:"turns"`
	Questions     int `json:"questions"`
	Skipped       int `json:"skipped"`
	// ImagesAttached counts turns whose image was fetched and inlined into the
	// message; ImagesFailed counts the ones that could not be fetched and fell
	// back to the caption annotation.
	ImagesAttached int `json:"images_attached,omitempty"`
	ImagesFailed   int `json:"images_failed,omitempty"`
	// SkippedAdversarial counts rows dropped because the dataset marks them
	// adversarial/unanswerable (LoCoMo category 5). It is reported apart from
	// Skipped so an excluded slice of the benchmark is never implicit.
	SkippedAdversarial int `json:"skipped_adversarial,omitempty"`
}

// adversarialCategory is LoCoMo's category 5. Those questions are unanswerable
// by construction: the conversation never states the answer and the dataset
// leaves the gold field empty, so the graded behaviour is a refusal rather than
// an answer. The harness reports answer accuracy over the answerable
// categories and drops this one explicitly.
const adversarialCategory = 5

// TemporalCategory is LoCoMo's category 2: questions that ask for a date. It is
// exported because the answering stage needs the same numbering to reproduce
// the reference harness's category-2 question suffix (see
// answer.WithTemporalHint), and the label is the only thing the two stages
// share -- the category never reaches retrieval.
const TemporalCategory = 2

// LoadLoCoMo converts snap-research/locomo locomo10.json into scenarios.
// Images are folded into a text annotation; gold answers become
// want_contains expectations graded against recalled context.
func LoadLoCoMo(data []byte, options LoaderOptions) ([]Scenario, LoaderStats, error) {
	var samples []loCoMoRawSample
	if err := json.Unmarshal(data, &samples); err != nil {
		return nil, LoaderStats{}, fmt.Errorf("memory eval: parse locomo: %w", err)
	}
	var (
		scenarios []Scenario
		stats     LoaderStats
	)
	fetcher := newImageFetcher()
	for _, sample := range samples {
		if options.Samples > 0 && stats.Conversations >= options.Samples {
			break
		}
		if strings.TrimSpace(sample.SampleID) == "" {
			stats.Skipped++
			continue
		}
		turns, ok := loCoMoTurns(sample, options.Images, fetcher, &stats)
		if !ok {
			stats.Skipped++
			continue
		}
		scenario := Scenario{
			Name: "locomo/" + sample.SampleID, Scope: options.Scope,
			ConversationID: sample.SampleID, Budget: options.Budget, Turns: turns,
		}
		stats.Conversations++
		stats.Turns += len(turns)
		for _, qa := range sample.QA {
			if qa.Category == adversarialCategory {
				// Explicit, counted exclusion (see adversarialCategory). Doing
				// it here rather than letting the empty-gold check below drop
				// these rows keeps the decision visible in the stats.
				stats.SkippedAdversarial++
				continue
			}
			question := strings.TrimSpace(qa.Question)
			answers := loCoMoAnswers(qa.Answer)
			if question == "" || len(answers) == 0 {
				stats.Skipped++
				continue
			}
			scenario.Questions = append(scenario.Questions, Question{
				Query: question, WantContains: answers,
				Evidence: append([]string(nil), qa.Evidence...), Category: qa.Category,
			})
			stats.Questions++
		}
		scenarios = append(scenarios, scenario)
	}
	return scenarios, stats, nil
}

// LoadLongMemEval converts xiaowu0162/longmemeval-cleaned JSON into
// scenarios. Sessions are reordered by haystack_dates so temporal questions
// see a coherent history.
func LoadLongMemEval(data []byte, options LoaderOptions) ([]Scenario, LoaderStats, error) {
	var instances []lmeRawInstance
	if err := json.Unmarshal(data, &instances); err != nil {
		return nil, LoaderStats{}, fmt.Errorf("memory eval: parse longmemeval: %w", err)
	}
	var (
		scenarios []Scenario
		stats     LoaderStats
	)
	for _, instance := range instances {
		if strings.TrimSpace(instance.QuestionID) == "" || len(instance.HaystackSessions) == 0 {
			stats.Skipped++
			continue
		}
		turns := lmeTurns(instance)
		if len(turns) == 0 {
			stats.Skipped++
			continue
		}
		scenario := Scenario{
			Name: "longmemeval/" + instance.QuestionID, Scope: options.Scope,
			ConversationID: instance.QuestionID, Budget: options.Budget, Turns: turns,
		}
		stats.Conversations++
		stats.Turns += len(turns)
		query := strings.TrimSpace(instance.Question)
		answer := lmeAnswer(instance.Answer)
		if query == "" || answer == "" {
			stats.Skipped++
			continue
		}
		scenario.Questions = append(scenario.Questions, Question{Query: query, WantContains: []string{answer}})
		stats.Questions++
		scenarios = append(scenarios, scenario)
	}
	return scenarios, stats, nil
}

type loCoMoRawTurn struct {
	Speaker     string   `json:"speaker"`
	DiaID       string   `json:"dia_id"`
	Text        string   `json:"text"`
	Query       string   `json:"query,omitempty"`
	BlipCaption string   `json:"blip_caption,omitempty"`
	ImgURL      []string `json:"img_url,omitempty"`
}

type loCoMoRawQA struct {
	Question string   `json:"question"`
	Answer   any      `json:"answer"`
	Evidence []string `json:"evidence,omitempty"`
	Category int      `json:"category"`
}

type loCoMoRawSample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []loCoMoRawQA              `json:"qa"`
}

func loCoMoTurns(sample loCoMoRawSample, imageMode string, fetcher *imageFetcher, stats *LoaderStats) ([]Turn, bool) {
	var speakerA, speakerB string
	_ = json.Unmarshal(sample.Conversation["speaker_a"], &speakerA)
	_ = json.Unmarshal(sample.Conversation["speaker_b"], &speakerB)
	type session struct {
		index    int
		key      string
		dateTime string
	}
	var sessions []session
	for key := range sample.Conversation {
		if !strings.HasPrefix(key, "session_") || strings.HasSuffix(key, "_date_time") {
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(key, "session_"))
		if err != nil {
			continue
		}
		var dateTime string
		_ = json.Unmarshal(sample.Conversation[key+"_date_time"], &dateTime)
		sessions = append(sessions, session{index: number, key: key, dateTime: dateTime})
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].index < sessions[j].index })
	turns := make([]Turn, 0, len(sessions))
	for _, current := range sessions {
		var rawTurns []loCoMoRawTurn
		if err := json.Unmarshal(sample.Conversation[current.key], &rawTurns); err != nil {
			continue
		}
		if imageMode == "native" || imageMode == "both" {
			urls := make([]string, 0, len(rawTurns))
			for _, raw := range rawTurns {
				for _, rawURL := range raw.ImgURL {
					if url := strings.TrimSpace(rawURL); url != "" {
						urls = append(urls, url)
					}
				}
			}
			fetcher.prefetch(urls)
		}
		messages := make([]coremessage.Message, 0, len(rawTurns))
		datasetIDs := make([]string, 0, len(rawTurns))
		for _, raw := range rawTurns {
			role := coremessage.RoleUser
			switch raw.Speaker {
			case speakerB:
				role = coremessage.RoleAssistant
			case speakerA:
				role = coremessage.RoleUser
			}
			body := strings.TrimSpace(raw.Text)
			if annotation := loCoMoImageAnnotation(raw); annotation != "" && imageMode != "native" {
				if body == "" {
					body = annotation
				} else {
					body += " " + annotation
				}
			}
			speaker := raw.Speaker
			if speaker == "" {
				speaker = string(role)
			}
			text := speaker + ": " + body
			if current.dateTime != "" {
				text = "[" + current.dateTime + "] " + text
			}
			parts := []coremessage.Part{coremessage.TextPart{Text: text}}
			if imageMode == "native" || imageMode == "both" {
				imageParts, ok := fetcher.parts(raw)
				if ok {
					parts = append(parts, imageParts...)
					stats.ImagesAttached++
				} else if len(raw.ImgURL) > 0 {
					stats.ImagesFailed++
					// Fall back to the caption so the turn is not lost.
					if annotation := loCoMoImageAnnotation(raw); annotation != "" {
						parts[0] = coremessage.TextPart{Text: text + " " + annotation}
					}
				}
			}
			if strings.TrimSpace(body) == "" && len(parts) == 1 {
				continue
			}
			messages = append(messages, coremessage.Message{
				Role: role, Content: coremessage.Content{Parts: parts},
			})
			datasetIDs = append(datasetIDs, raw.DiaID)
		}
		if len(messages) == 0 {
			continue
		}
		if !hasNonEmpty(datasetIDs) {
			datasetIDs = nil
		}
		turns = append(turns, Turn{
			IdempotencyKey: "locomo/" + sample.SampleID + "/" + current.key,
			Messages:       messages,
			DatasetIDs:     datasetIDs,
		})
	}
	return turns, len(turns) > 0
}

// prefetch downloads a conversation's images with a bounded pool. Sequential
// fetching made a smoke run spend minutes waiting on dead links one timeout at
// a time; the cache makes the later per-turn lookups free.
func (fetcher *imageFetcher) prefetch(urls []string) {
	if fetcher == nil || len(urls) == 0 {
		return
	}
	const workers = 8
	queue := make(chan string)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for url := range queue {
				_, _ = fetcher.image(url)
			}
		}()
	}
	for _, url := range urls {
		queue <- url
	}
	close(queue)
	group.Wait()
}

// maxCachedImages bounds the in-process image cache. Each image can be up to
// maxImageBytes, so an unbounded cache over LoCoMo's 910 image turns could hold
// gigabytes; the cache is an optimisation, not a store.
const maxCachedImages = 64

// maxImageBytes bounds one download; a turn's image is context, not a payload.
const maxImageBytes = 4 << 20

// imageFetcher downloads dataset images once and inlines the bytes. Passing the
// remote url through is not enough: the multimodal embedding endpoint refuses
// to fetch third-party hosts (measured: `provider_failure during embed` on
// LoCoMo's reddit/flickr links), so the bytes have to travel with the message.
//
// Downloads are cached on disk (not only in memory) because the committed
// messages are immutable: a run that reuses a workspace but downloads a
// different subset of images -- network luck, not configuration -- would build
// its evidence index from text the workspace does not contain, which silently
// understates recall. The disk cache makes the loader's output reproducible and
// removes the repeated multi-minute download.
type imageFetcher struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]fetchedImage
	dir    string
}

type fetchedImage struct {
	parts []coremessage.Part
	err   error
}

func newImageFetcher() *imageFetcher {
	dir := filepath.Join(os.TempDir(), "flowcraft-image-cache")
	_ = os.MkdirAll(dir, 0o700)
	return &imageFetcher{
		client: &http.Client{Timeout: 5 * time.Second},
		cache:  map[string]fetchedImage{},
		dir:    dir,
	}
}

// parts returns the inlined image parts of one turn, reporting false when none
// of its urls could be materialized (the caller then keeps the caption).
func (fetcher *imageFetcher) parts(turn loCoMoRawTurn) ([]coremessage.Part, bool) {
	if fetcher == nil || len(turn.ImgURL) == 0 {
		return nil, false
	}
	parts := make([]coremessage.Part, 0, len(turn.ImgURL))
	for _, rawURL := range turn.ImgURL {
		url := strings.TrimSpace(rawURL)
		if url == "" {
			continue
		}
		if fetched, err := fetcher.image(url); err == nil {
			parts = append(parts, fetched...)
		}
	}
	return parts, len(parts) > 0
}

func (fetcher *imageFetcher) image(url string) ([]coremessage.Part, error) {
	fetcher.mu.Lock()
	cached, exists := fetcher.cache[url]
	fetcher.mu.Unlock()
	if exists {
		return cached.parts, cached.err
	}
	if parts, err, ok := fetcher.loadCached(url); ok {
		// Only successes go into the in-memory cache: caching an empty result
		// here would outlive the disk TTL and block every later retry.
		if len(parts) > 0 {
			fetcher.mu.Lock()
			if len(fetcher.cache) < maxCachedImages {
				fetcher.cache[url] = fetchedImage{parts: parts}
			}
			fetcher.mu.Unlock()
		}
		return parts, err
	}
	parts, err := fetcher.download(url)
	if err != nil {
		// Remember the failure so the run-to-run image set is stable.
		_ = os.WriteFile(fetcher.cachedPath(url), []byte(failureMarker), 0o600)
	}
	if err == nil {
		// Cache hits only: a failed fetch is retried on the next run instead of
		// being remembered, and the cache stays bounded.
		fetcher.storeCached(url, parts)
		fetcher.mu.Lock()
		if len(fetcher.cache) < maxCachedImages {
			fetcher.cache[url] = fetchedImage{parts: parts}
		}
		fetcher.mu.Unlock()
	}
	return parts, err
}

// cachedPath names the disk entry for one url.
// errImageUnavailable marks a url whose failure was cached.
var errImageUnavailable = errors.New("image fetch: cached unavailable")

func (fetcher *imageFetcher) cachedPath(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(fetcher.dir, hex.EncodeToString(sum[:])+".img")
}

// failureMarker records that a url could not be materialized, so later runs make
// the same decision instead of depending on network luck.
const failureMarker = "!failed"

// failureTTL keeps a cached failure long enough for a batch of experiments to
// see the same image set, then lets the url be retried.
const failureTTL = 24 * time.Hour

// loadCached reads a previously fetched image, if the bytes are on disk.
func (fetcher *imageFetcher) loadCached(url string) ([]coremessage.Part, error, bool) {
	if fetcher == nil || fetcher.dir == "" {
		return nil, nil, false
	}
	data, err := os.ReadFile(fetcher.cachedPath(url))
	if err != nil || len(data) == 0 {
		return nil, nil, false
	}
	if string(data) == failureMarker {
		info, statErr := os.Stat(fetcher.cachedPath(url))
		if statErr == nil && time.Since(info.ModTime()) < failureTTL {
			return nil, errImageUnavailable, true
		}
		// Stale marker: try the network again.
		return nil, nil, false
	}
	mediaType := imageSignature(data)
	if mediaType == "" {
		return nil, nil, false
	}
	source, err := media.NewImageBytes(data, mediaType)
	if err != nil {
		return nil, nil, false
	}
	return []coremessage.Part{coremessage.ImagePart{Source: source}}, nil, true
}

// storeCached writes the fetched bytes so later runs reuse them.
func (fetcher *imageFetcher) storeCached(url string, parts []coremessage.Part) {
	if fetcher == nil || fetcher.dir == "" || len(parts) == 0 {
		return
	}
	imagePart, ok := parts[0].(coremessage.ImagePart)
	if !ok {
		return
	}
	data := imagePart.Source.Bytes()
	if len(data) == 0 {
		return
	}
	_ = os.WriteFile(fetcher.cachedPath(url), data, 0o600)
}

func (fetcher *imageFetcher) download(url string) ([]coremessage.Part, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "flowcraft-memory-eval/1.0")
	response, err := fetcher.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image fetch: status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return nil, fmt.Errorf("image fetch: %d bytes", len(data))
	}
	mediaType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	// Trust the bytes, not the header: some hosts answer a hotlink with an HTML
	// block page under an image content type, and those are exactly the
	// payloads the multimodal endpoint refuses. Anything without a real image
	// signature falls back to the caption.
	signature := imageSignature(data)
	if signature == "" {
		return nil, fmt.Errorf("image fetch: unrecognized payload (%s)", mediaType)
	}
	if !strings.HasPrefix(mediaType, "image/") || mediaType != signature {
		mediaType = signature
	}
	source, err := media.NewImageBytes(data, mediaType)
	if err != nil {
		return nil, err
	}
	return []coremessage.Part{coremessage.ImagePart{Source: source}}, nil
}

// imageSignature reports the media type of a payload from its magic bytes, or
// "" when it is not an image we can hand to the multimodal endpoint.
func imageSignature(data []byte) string {
	switch {
	case len(data) > 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) > 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) > 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) > 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

func loCoMoImageAnnotation(turn loCoMoRawTurn) string {
	if len(turn.ImgURL) == 0 {
		return ""
	}
	hint := strings.TrimSpace(turn.Query)
	if hint == "" {
		hint = strings.TrimSpace(turn.BlipCaption)
	}
	if hint == "" {
		return ""
	}
	return "[shared image: " + hint + "]"
}

func hasNonEmpty(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func loCoMoAnswers(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		return []string{trimmed}
	case float64:
		return []string{strconv.FormatFloat(typed, 'f', -1, 64)}
	case json.Number:
		return []string{typed.String()}
	case []any:
		var answers []string
		for _, item := range typed {
			answers = append(answers, loCoMoAnswers(item)...)
		}
		return answers
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return nil
		}
		return []string{string(encoded)}
	}
}

type lmeRawTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	HasAnswer bool   `json:"has_answer,omitempty"`
}

type lmeRawInstance struct {
	QuestionID         string          `json:"question_id"`
	QuestionType       string          `json:"question_type"`
	Question           string          `json:"question"`
	Answer             json.RawMessage `json:"answer"`
	QuestionDate       string          `json:"question_date"`
	HaystackSessionIDs []string        `json:"haystack_session_ids"`
	HaystackDates      []string        `json:"haystack_dates"`
	HaystackSessions   [][]lmeRawTurn  `json:"haystack_sessions"`
}

func lmeTurns(instance lmeRawInstance) []Turn {
	indexes := make([]int, len(instance.HaystackSessions))
	for index := range indexes {
		indexes[index] = index
	}
	sort.SliceStable(indexes, func(i, j int) bool {
		left, right := lmeDate(instance, indexes[i]), lmeDate(instance, indexes[j])
		if left != right {
			return left < right
		}
		return indexes[i] < indexes[j]
	})
	turns := make([]Turn, 0, len(indexes))
	for _, index := range indexes {
		messages := make([]coremessage.Message, 0, len(instance.HaystackSessions[index]))
		for _, raw := range instance.HaystackSessions[index] {
			text := strings.TrimSpace(raw.Content)
			if text == "" {
				continue
			}
			role := coremessage.RoleUser
			if strings.EqualFold(raw.Role, "assistant") {
				role = coremessage.RoleAssistant
			}
			messages = append(messages, coremessage.NewTextMessage(role, text))
		}
		if len(messages) == 0 {
			continue
		}
		sessionID := strconv.Itoa(index)
		if index < len(instance.HaystackSessionIDs) && instance.HaystackSessionIDs[index] != "" {
			sessionID = instance.HaystackSessionIDs[index]
		}
		turns = append(turns, Turn{
			IdempotencyKey: "longmemeval/" + instance.QuestionID + "/" + sessionID,
			Messages:       messages,
		})
	}
	return turns
}

func lmeDate(instance lmeRawInstance, index int) string {
	if index < len(instance.HaystackDates) {
		return instance.HaystackDates[index]
	}
	return ""
}

func lmeAnswer(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err == nil {
		return strings.TrimSpace(strings.Join(values, " "))
	}
	return strings.TrimSpace(string(raw))
}
