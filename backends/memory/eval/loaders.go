package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
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
	// ReferenceImages attaches each image url as the part instead of downloading
	// what it points at. The text keeps the shape Images asks for, so a caller
	// that reads the text -- the diagnostic's index, a conversion counted before
	// a run -- gets the turn as a store would hold it, without the network in
	// the result. The difference is the bytes: a part carrying a url is not a
	// picture anything has fetched, so a store ingested this way holds a
	// reference. It is the shape of a native store whose fetches all succeeded,
	// which is the one a diagnostic wants to compare against.
	ReferenceImages bool
	// Dateless converts turns without the session date prefix the LoCoMo
	// rendering writes into every message ("[1:00 pm on 1 May, 2023] Alice:
	// ..."). Nothing else about a turn changes -- speaker, body, dataset id,
	// image annotation stay what they were -- so a run over each conversion
	// measures what the prefix contributes to a temporal score, with the
	// turns themselves held fixed.
	Dateless bool
	// ObservedAt selects whether the conversion writes each session date time
	// into the messages as the observed_at metadata anchor instead of their
	// text:
	//   "" or "off"  -- no anchor (the historical conversion, byte for byte)
	//   "metadata"   -- every message of a session carries the session date
	//                   time as an RFC3339 observed_at; requires Dateless,
	//                   because the metadata anchor and the text prefix are
	//                   two renderings of the same fact and a turn carrying
	//                   both would reach derivation with two anchors.
	ObservedAt string
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
	// ImagesShrunk counts fetched payloads that were re-encoded to fit the
	// inline budget, so a run says when the pictures it embedded are not the
	// bytes the dataset serves.
	ImagesShrunk int `json:"images_shrunk,omitempty"`
	// ImagesReferenced counts turns whose image was attached as a url instead
	// of downloaded (LoaderOptions.ReferenceImages). It is reported apart from
	// ImagesAttached because no picture was embedded: the message names one.
	ImagesReferenced int `json:"images_referenced,omitempty"`
	// ImageFailures breaks the failed fetches down by cause: a dead link and a
	// timeout both end as "no image", but only one of them is worth retrying.
	ImageFailures ImageFailureCounts `json:"image_failures,omitzero"`
	// SkippedAdversarial counts rows dropped because the dataset marks them
	// adversarial/unanswerable (LoCoMo category 5). It is reported apart from
	// Skipped so an excluded slice of the benchmark is never implicit.
	SkippedAdversarial int `json:"skipped_adversarial,omitempty"`
	// ObservedAtAnchored counts the messages whose conversion wrote an
	// observed_at anchor into their metadata, and ObservedAtUnparsed
	// counts the sessions whose date string the conversion could not
	// parse -- an unparsable date leaves the messages without an anchor
	// rather than guessing one. Both are reported so anchor coverage is
	// a reading on the run, not an assumption.
	ObservedAtAnchored int `json:"observed_at_anchored,omitempty"`
	ObservedAtUnparsed int `json:"observed_at_unparsed,omitempty"`
	// EvidenceUnresolved counts gold evidence references the conversion could
	// not resolve against the conversation it rendered: a dataset that names a
	// session (LongMemEval) or a turn (LoCoMo) the haystack does not hold. The
	// reference is dropped from the question's evidence instead of grading as a
	// miss no retrieval could ever fix, and counted here so the drop is never
	// silent.
	EvidenceUnresolved int `json:"evidence_unresolved,omitempty"`
}

// ImageFailureCounts breaks the failed image fetches down by cause. A failure
// that describes the url (a dead link, a hotlink block, a payload too large to
// inline) is cached and reported as permanent; one that describes the moment (a
// timeout, a refused connection, a 5xx) is retried. Lumping them together is
// what made an image set a function of the network's luck.
type ImageFailureCounts struct {
	Permanent int `json:"permanent,omitempty"`
	Busy      int `json:"busy,omitempty"`
	Transient int `json:"transient,omitempty"`
	Oversized int `json:"oversized,omitempty"`
}

func (counts *ImageFailureCounts) add(kind imageFailureKind) {
	switch kind {
	case failurePermanent:
		counts.Permanent++
	case failureBusy:
		counts.Busy++
	case failureOversized:
		counts.Oversized++
	default:
		counts.Transient++
	}
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
	observedAt, err := options.observedAtMode()
	if err != nil {
		return nil, LoaderStats{}, err
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
		turns, ok := loCoMoTurns(sample, imagePolicy{
			mode:       options.Images,
			references: options.ReferenceImages,
		}, options.Dateless, observedAt == "metadata", fetcher, &stats)
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

// LoadLongMemEval converts xiaowu0162/longmemeval-cleaned JSON into scenarios.
//
// Three dataset facts shape the conversion. Sessions are reordered by
// haystack_dates so temporal questions see a coherent history. Each session
// carries a date that the asking side needs -- a question phrased "last year"
// cannot be resolved from an undated transcript -- so the conversion writes it
// the way the LoCoMo conversion does, under the same -no-date-prefix /
// -observed-at switches. And the dataset names its evidence by session rather
// than by turn, so Question.EvidenceSessions carries the gold sessions while
// Turn.DatasetIDs still names every message, which is what lets a recalled
// message be matched back to the session that holds the answer.
func LoadLongMemEval(data []byte, options LoaderOptions) ([]Scenario, LoaderStats, error) {
	var instances []lmeRawInstance
	if err := json.Unmarshal(data, &instances); err != nil {
		return nil, LoaderStats{}, fmt.Errorf("memory eval: parse longmemeval: %w", err)
	}
	observedAt, err := options.observedAtMode()
	if err != nil {
		return nil, LoaderStats{}, err
	}
	render := lmeRender{dateless: options.Dateless, observedAt: observedAt == "metadata"}
	var (
		scenarios []Scenario
		stats     LoaderStats
	)
	for _, instance := range instances {
		if strings.TrimSpace(instance.QuestionID) == "" || len(instance.HaystackSessions) == 0 {
			stats.Skipped++
			continue
		}
		sessions := lmeSessions(instance, render, &stats)
		if len(sessions) == 0 {
			stats.Skipped++
			continue
		}
		scenario := Scenario{
			Name: "longmemeval/" + instance.QuestionID, Scope: options.Scope,
			ConversationID: instance.QuestionID, Budget: options.Budget,
		}
		known := make(map[string]struct{}, len(sessions))
		for _, session := range sessions {
			scenario.Turns = append(scenario.Turns, session.turn(instance.QuestionID))
			known[session.id] = struct{}{}
		}
		stats.Conversations++
		stats.Turns += len(scenario.Turns)
		query := strings.TrimSpace(instance.Question)
		answer := lmeAnswer(instance.Answer)
		if query == "" || answer == "" {
			stats.Skipped++
			continue
		}
		scenario.Questions = append(scenario.Questions, Question{
			Query: query, WantContains: []string{answer},
			EvidenceSessions: lmeEvidenceSessions(instance.AnswerSessionIDs, known, &stats),
			AskedAt:          strings.TrimSpace(instance.QuestionDate),
			Category:         lmeCategory(instance.QuestionType),
			CategoryLabel:    strings.TrimSpace(instance.QuestionType),
			// The abstention rows are the dataset's own marker for a question
			// its history cannot answer: the graded behaviour is saying so,
			// not producing the fact.
			Unanswerable: strings.HasSuffix(strings.TrimSpace(instance.QuestionID), longMemEvalAbstentionSuffix),
		})
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

// imagePolicy is how one conversion treats the images a conversation names:
// whether a turn's text folds its caption in, whether the picture rides along as
// a message part, and whether that part is the payload or a reference to it.
type imagePolicy struct {
	mode       string
	references bool
}

// foldsCaption reports whether the caption joins the turn's text. "native" is
// the mode that keeps it out, which is what makes a native store and the
// loader's own rendering disagree about an image turn's text.
func (policy imagePolicy) foldsCaption() bool { return policy.mode != "native" }

// attaches reports whether the picture rides along as a message part.
func (policy imagePolicy) attaches() bool {
	return policy.mode == "native" || policy.mode == "both"
}

// observedAtMode normalizes LoaderOptions.ObservedAt and enforces its pairing
// with Dateless: the metadata anchor replaces the text prefix, so a
// conversion that would carry both is rejected rather than silently
// doubling the anchor.
func (options LoaderOptions) observedAtMode() (string, error) {
	switch strings.TrimSpace(options.ObservedAt) {
	case "", "off":
		return "off", nil
	case "metadata":
		if !options.Dateless {
			return "", errors.New("memory eval: observed_at=metadata writes the anchor into metadata instead of the text, so it requires the dateless conversion")
		}
		return "metadata", nil
	default:
		return "", fmt.Errorf("memory eval: unknown observed_at mode %q (want off|metadata)", options.ObservedAt)
	}
}

// loCoMoDateTimeLayout is the shape of every session date_time value in
// locomo10.json: "1:56 pm on 8 May, 2023". A value in no known layout is
// left without an anchor rather than guessed at.
const loCoMoDateTimeLayout = "3:04 pm on 2 January, 2006"

func parseLoCoMoDateTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(loCoMoDateTimeLayout, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func loCoMoTurns(sample loCoMoRawSample, policy imagePolicy, dateless, observedAt bool, fetcher *imageFetcher, stats *LoaderStats) ([]Turn, bool) {
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
		// The observation anchor is a property of the session: every
		// message exchanged in one session was observed at the session
		// date time, so the conversion stamps them all with it.
		sessionAnchor := ""
		if observedAt && strings.TrimSpace(current.dateTime) != "" {
			if parsed, ok := parseLoCoMoDateTime(current.dateTime); ok {
				sessionAnchor = parsed.Format(time.RFC3339)
			} else {
				stats.ObservedAtUnparsed++
			}
		}
		// Only a conversion that downloads needs the pool: a reference
		// conversion has nothing to wait for, and fetching a picture it will
		// never inline is how a run with no use for the bytes spends minutes
		// on dead links.
		if policy.attaches() && !policy.references {
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
		observedAts := make([]string, 0, len(rawTurns))
		for _, raw := range rawTurns {
			role := coremessage.RoleUser
			switch raw.Speaker {
			case speakerB:
				role = coremessage.RoleAssistant
			case speakerA:
				role = coremessage.RoleUser
			}
			body := strings.TrimSpace(raw.Text)
			if annotation := loCoMoImageAnnotation(raw); annotation != "" && policy.foldsCaption() {
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
			// The date is dataset content, but carrying it in the text is
			// the loader's choice; the dateless conversion drops exactly
			// this step.
			if current.dateTime != "" && !dateless {
				text = "[" + current.dateTime + "] " + text
			}
			parts := []coremessage.Part{coremessage.TextPart{Text: text}}
			if policy.attaches() && policy.references {
				references := imageReferences(raw)
				if len(references) > 0 {
					parts = append(parts, references...)
					stats.ImagesReferenced++
				}
			} else if policy.attaches() {
				imageParts, imageErr := fetcher.parts(raw)
				if imageErr == nil && len(imageParts) > 0 {
					parts = append(parts, imageParts...)
					stats.ImagesAttached++
				} else if len(raw.ImgURL) > 0 {
					stats.ImagesFailed++
					if imageErr != nil {
						kind, _ := failureKind(imageErr)
						stats.ImageFailures.add(kind)
					}
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
			observedAts = append(observedAts, sessionAnchor)
			if sessionAnchor != "" {
				stats.ObservedAtAnchored++
			}
		}
		if len(messages) == 0 {
			continue
		}
		if !hasNonEmpty(datasetIDs) {
			datasetIDs = nil
		}
		if !hasNonEmpty(observedAts) {
			observedAts = nil
		}
		turns = append(turns, Turn{
			IdempotencyKey: "locomo/" + sample.SampleID + "/" + current.key,
			Messages:       messages,
			DatasetIDs:     datasetIDs,
			ObservedAt:     observedAts,
		})
	}
	stats.ImagesShrunk = fetcher.shrunkCount()
	return turns, len(turns) > 0
}

// imageReferences turns a turn's image urls into parts that name them instead of
// carrying them. The media type is left empty on purpose: nothing has looked at
// the bytes, and a guess from the url's extension is a claim the loader cannot
// make. A url the media package rejects is left out rather than failing the
// turn -- the same trade the fetcher makes for a payload it cannot use.
func imageReferences(turn loCoMoRawTurn) []coremessage.Part {
	parts := make([]coremessage.Part, 0, len(turn.ImgURL))
	for _, rawURL := range turn.ImgURL {
		url := strings.TrimSpace(rawURL)
		if url == "" {
			continue
		}
		source, err := media.NewImageURL(url, "")
		if err != nil {
			continue
		}
		parts = append(parts, coremessage.ImagePart{Source: source})
	}
	return parts
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

// maxImageBytes bounds what one turn inlines; a turn's image is context, not a
// payload.
const maxImageBytes = 4 << 20

// maxDownloadBytes bounds what a fetch holds in memory in order to consider
// shrinking it: decoding a JPEG needs roughly 20 bytes of bitmap per byte of
// file, so the ceiling is what keeps pathological payloads from deciding a run's
// memory. A payload beyond it is a property of the url rather than a slow moment,
// so retrying it every run buys nothing. The largest image in the benchmark is a
// 4.9MB Wikipedia original.
const maxDownloadBytes = 12 << 20

// imageTimeout is one request's budget. The old 5s was too tight for the
// benchmark's long tail of slow hosts, and because every failure used to be
// cached regardless of cause, a timeout became a missing image for the next
// 24h: "slow" was recorded as "gone".
const imageTimeout = 20 * time.Second

// imageAttempts is how many times one url is tried when the server answers with
// a transient status (429, 5xx). Transport failures are not retried: a second
// 20s wait on a dead host only makes the pass slower.
const imageAttempts = 2

// imageUserAgent identifies the fetcher. Some hosts (Wikipedia in particular)
// answer an unidentified client with 403.
const imageUserAgent = "flowcraft-memory-eval/1.0"

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
	// shrunk counts payloads re-encoded to fit maxImageBytes, so a run says
	// when the bytes it embedded are not the bytes the dataset serves.
	shrunk int
}

type fetchedImage struct {
	parts []coremessage.Part
	err   error
}

func newImageFetcher() *imageFetcher {
	dir := filepath.Join(os.TempDir(), "flowcraft-image-cache")
	_ = os.MkdirAll(dir, 0o700)
	return &imageFetcher{
		client: &http.Client{Timeout: imageTimeout},
		cache:  map[string]fetchedImage{},
		dir:    dir,
	}
}

// shrunkCount reports how many payloads were re-encoded to fit the inline
// budget.
func (fetcher *imageFetcher) shrunkCount() int {
	if fetcher == nil {
		return 0
	}
	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()
	return fetcher.shrunk
}

// parts returns the inlined image parts of one turn and the reason none of its
// urls could be materialized, so the caller can both keep the caption and say
// why the picture is missing.
func (fetcher *imageFetcher) parts(turn loCoMoRawTurn) ([]coremessage.Part, error) {
	if fetcher == nil || len(turn.ImgURL) == 0 {
		return nil, nil
	}
	parts := make([]coremessage.Part, 0, len(turn.ImgURL))
	var firstErr error
	for _, rawURL := range turn.ImgURL {
		url := strings.TrimSpace(rawURL)
		if url == "" {
			continue
		}
		fetched, err := fetcher.image(url)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		parts = append(parts, fetched...)
	}
	if len(parts) > 0 {
		return parts, nil
	}
	if firstErr == nil {
		firstErr = &fetchError{kind: failurePermanent, detail: "no image url"}
	}
	return nil, firstErr
}

func (fetcher *imageFetcher) image(url string) ([]coremessage.Part, error) {
	fetcher.mu.Lock()
	cached, exists := fetcher.cache[url]
	fetcher.mu.Unlock()
	if exists {
		return cached.parts, cached.err
	}
	if parts, err, ok := fetcher.loadCached(url); ok {
		fetcher.remember(url, parts, err)
		return parts, err
	}
	parts, err := fetcher.fetch(url)
	if err != nil {
		fetcher.storeFailure(url, err)
	}
	fetcher.remember(url, parts, err)
	return parts, err
}

// fetch performs the network round trip and, on success, materializes and
// stores the payload.
func (fetcher *imageFetcher) fetch(url string) ([]coremessage.Part, error) {
	data, mediaType, err := fetcher.download(url)
	if err != nil {
		return nil, err
	}
	parts, err := fetcher.materialize(data, mediaType)
	if err != nil {
		return nil, err
	}
	fetcher.storeCached(url, parts)
	return parts, nil
}

// remember memoizes one attempt for the rest of the process, successes and
// failures alike: a url used by several turns must not be re-fetched (or
// re-waited-on) per turn. The cross-run decision lives in the disk cache, not
// here -- this map is bounded and dies with the run.
func (fetcher *imageFetcher) remember(url string, parts []coremessage.Part, err error) {
	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()
	if len(fetcher.cache) < maxCachedImages {
		fetcher.cache[url] = fetchedImage{parts: parts, err: err}
	}
}

// cachedPath names the disk entry for one url.
func (fetcher *imageFetcher) cachedPath(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(fetcher.dir, hex.EncodeToString(sum[:])+".img")
}

// failureMarkerPrefix opens a disk record of a url that could not be
// materialized: "!failed <kind> <detail>". Only a cause that describes the url
// is written, so later runs report the same reason without a network round trip
// -- while a timeout or a 5xx is retried instead of being frozen.
const failureMarkerPrefix = "!failed "

// failureTTL keeps a cached failure long enough for a batch of experiments to
// see the same image set, then lets the url be retried.
const failureTTL = 24 * time.Hour

// imageFailureKind says whether a failed fetch describes the url or the moment.
type imageFailureKind string

const (
	// failurePermanent is a url that will not work within the batch: a dead
	// link, a hotlink block, a payload that is not an image.
	failurePermanent imageFailureKind = "permanent"
	// failureBusy is a server-side transient: 408, 425, 429, 5xx.
	failureBusy imageFailureKind = "busy"
	// failureTransient is a transport-level transient: a timeout, a refused
	// connection, a reset stream.
	failureTransient imageFailureKind = "transient"
	// failureOversized is a real image bigger than the inline budget, whether
	// or not re-encoding could shrink it.
	failureOversized imageFailureKind = "oversized"
)

// cachedFailure reports whether a failure describes the url itself and is
// therefore worth remembering across runs. A "busy" or "transient" failure is
// not: recording one for 24h is how a slow host became a missing image.
func cachedFailure(kind imageFailureKind) bool {
	return kind == failurePermanent || kind == failureOversized
}

// fetchError carries the classification with the cause.
type fetchError struct {
	kind   imageFailureKind
	detail string
	err    error
}

func (failure *fetchError) Error() string {
	if failure.detail == "" {
		return "image fetch: " + string(failure.kind)
	}
	return "image fetch: " + string(failure.kind) + ": " + failure.detail
}

func (failure *fetchError) Unwrap() error { return failure.err }

// failureKind reads the classification out of a fetch error. An unrecognized
// error is reported as permanent: retrying an unknown cause every run is the
// behaviour that produced runs which could not be compared.
func failureKind(err error) (imageFailureKind, string) {
	var failure *fetchError
	if errors.As(err, &failure) {
		return failure.kind, failure.detail
	}
	if err == nil {
		return "", ""
	}
	return failurePermanent, err.Error()
}

// storeFailure records a url whose failure describes the url itself. A
// transient failure is deliberately not written: freezing one for the next 24h
// is what made the benchmark's image set depend on the network's luck.
func (fetcher *imageFetcher) storeFailure(url string, err error) {
	kind, detail := failureKind(err)
	if fetcher == nil || fetcher.dir == "" || !cachedFailure(kind) {
		return
	}
	_ = os.WriteFile(fetcher.cachedPath(url), []byte(failureMarkerPrefix+string(kind)+" "+detail), 0o600)
}

// loadCached reads a previously fetched image, if the bytes are on disk.
func (fetcher *imageFetcher) loadCached(url string) ([]coremessage.Part, error, bool) {
	if fetcher == nil || fetcher.dir == "" {
		return nil, nil, false
	}
	data, err := os.ReadFile(fetcher.cachedPath(url))
	if err != nil || len(data) == 0 {
		return nil, nil, false
	}
	if strings.HasPrefix(string(data), failureMarkerPrefix) {
		info, statErr := os.Stat(fetcher.cachedPath(url))
		if statErr == nil && time.Since(info.ModTime()) < failureTTL {
			kind, detail, _ := strings.Cut(strings.TrimPrefix(string(data), failureMarkerPrefix), " ")
			return nil, &fetchError{kind: imageFailureKind(kind), detail: detail}, true
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

// download fetches one url's bytes, retrying a server-side transient. The error
// is always a *fetchError, so the caller can tell a dead link from a busy
// server from a slow moment.
func (fetcher *imageFetcher) download(url string) ([]byte, string, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", &fetchError{kind: failurePermanent, detail: err.Error(), err: err}
	}
	request.Header.Set("User-Agent", imageUserAgent)
	var last error
	for attempt := 1; attempt <= imageAttempts; attempt++ {
		data, mediaType, err := fetcher.fetchOnce(request)
		if err == nil {
			return data, mediaType, nil
		}
		last = err
		if kind, _ := failureKind(err); kind != failureBusy {
			break
		}
		if attempt < imageAttempts {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
	}
	return nil, "", last
}

func (fetcher *imageFetcher) fetchOnce(request *http.Request) ([]byte, string, error) {
	response, err := fetcher.client.Do(request)
	if err != nil {
		return nil, "", classifyTransportError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, "", classifyStatus(response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, "", &fetchError{kind: failureTransient, detail: err.Error(), err: err}
	}
	if len(data) == 0 {
		return nil, "", &fetchError{kind: failurePermanent, detail: "empty payload"}
	}
	if len(data) > maxDownloadBytes {
		return nil, "", &fetchError{
			kind:   failureOversized,
			detail: fmt.Sprintf("over %d bytes", maxDownloadBytes),
		}
	}
	return data, strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]), nil
}

// classifyTransportError reads a transport failure. A name that does not resolve
// will not resolve on the next run either, while a timeout or a refused
// connection is a property of the moment.
func classifyTransportError(err error) error {
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return &fetchError{kind: failurePermanent, detail: "dns: " + dns.Name, err: err}
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return &fetchError{kind: failureTransient, detail: "timeout", err: err}
	}
	return &fetchError{kind: failureTransient, detail: err.Error(), err: err}
}

// classifyStatus reads an HTTP status: a server-side transient is worth a
// retry, a 4xx is a statement about the url.
func classifyStatus(status int) error {
	detail := fmt.Sprintf("status %d", status)
	switch {
	case status == http.StatusRequestTimeout, status == http.StatusTooEarly,
		status == http.StatusTooManyRequests, status >= 500:
		return &fetchError{kind: failureBusy, detail: detail}
	case status >= 400:
		return &fetchError{kind: failurePermanent, detail: detail}
	default:
		return &fetchError{kind: failureTransient, detail: detail}
	}
}

// materialize turns one downloaded payload into an image part, shrinking it when
// it does not fit the inline budget. Dropping an oversized image used to lose
// the turn's picture entirely -- only its caption survived -- and the benchmark
// has real ones: its largest image is a 4.9MB Wikipedia original, which no
// caption stands in for.
func (fetcher *imageFetcher) materialize(data []byte, mediaType string) ([]coremessage.Part, error) {
	// Trust the bytes, not the header: some hosts answer a hotlink with an HTML
	// block page under an image content type, and those are exactly the
	// payloads the multimodal endpoint refuses.
	signature := imageSignature(data)
	if signature == "" {
		return nil, &fetchError{
			kind:   failurePermanent,
			detail: fmt.Sprintf("unrecognized payload (%s)", mediaType),
		}
	}
	mediaType = signature
	if len(data) > maxImageBytes {
		smaller, err := shrinkImage(data)
		if err != nil {
			return nil, &fetchError{
				kind:   failureOversized,
				detail: fmt.Sprintf("%d bytes: %v", len(data), err),
			}
		}
		data, mediaType = smaller, "image/jpeg"
		fetcher.mu.Lock()
		fetcher.shrunk++
		fetcher.mu.Unlock()
	}
	if len(data) > maxImageBytes {
		return nil, &fetchError{
			kind:   failureOversized,
			detail: fmt.Sprintf("%d bytes after re-encode", len(data)),
		}
	}
	source, err := media.NewImageBytes(data, mediaType)
	if err != nil {
		return nil, &fetchError{kind: failurePermanent, detail: err.Error(), err: err}
	}
	return []coremessage.Part{coremessage.ImagePart{Source: source}}, nil
}

// shrinkImage re-encodes an oversized payload as JPEG, halving its dimensions
// until it fits. Only the standard library's codecs are available, so an
// oversized WebP keeps its caption. Re-encoding is a deliberate loss of
// fidelity: the alternative is losing the image, and a smaller picture is closer
// to what the dataset shows than a sentence about it.
func shrinkImage(data []byte) ([]byte, error) {
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	current := decoded
	for attempt := 0; ; attempt++ {
		encoded, err := encodeJPEG(current)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= maxImageBytes {
			return encoded, nil
		}
		if attempt >= 4 {
			return nil, fmt.Errorf("still %d bytes after 4 halvings", len(encoded))
		}
		halved := scaleHalf(current)
		if halved == nil {
			return nil, fmt.Errorf("too small to halve at %d bytes", len(encoded))
		}
		current = halved
	}
}

func encodeJPEG(source image.Image) ([]byte, error) {
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, source, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// scaleHalf halves an image with a 2x2 box filter. The standard library can
// decode but not resample, and a nearest-neighbour copy would alias badly on the
// photographs this shrinks.
func scaleHalf(source image.Image) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx()/2, bounds.Dy()/2
	if width < 16 || height < 16 {
		return nil
	}
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var red, green, blue, alpha uint32
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					r, g, b, a := source.At(bounds.Min.X+2*x+dx, bounds.Min.Y+2*y+dy).RGBA()
					red, green, blue, alpha = red+r, green+g, blue+b, alpha+a
				}
			}
			target.SetRGBA(x, y, color.RGBA{
				R: uint8(red / 4 >> 8), G: uint8(green / 4 >> 8),
				B: uint8(blue / 4 >> 8), A: uint8(alpha / 4 >> 8),
			})
		}
	}
	return target
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
	AnswerSessionIDs   []string        `json:"answer_session_ids"`
	HaystackSessionIDs []string        `json:"haystack_session_ids"`
	HaystackDates      []string        `json:"haystack_dates"`
	HaystackSessions   [][]lmeRawTurn  `json:"haystack_sessions"`
}

// LongMemEval category ids. The numbering starts above LoCoMo's 1..5 on
// purpose: TemporalCategory (2) is LoCoMo's category and it decides whether the
// answering stage appends the reference harness's question suffix, so a
// LongMemEval question must never land on it. A report says which dataset a
// bucket came from through Question.CategoryLabel rather than through the
// number alone.
const (
	LongMemEvalMultiSession            = 101
	LongMemEvalTemporalReasoning       = 102
	LongMemEvalKnowledgeUpdate         = 103
	LongMemEvalSingleSessionUser       = 104
	LongMemEvalSingleSessionAssistant  = 105
	LongMemEvalSingleSessionPreference = 106
	// LongMemEvalUnknownCategory is the bucket for a question_type this build
	// does not know. It keeps the question graded (the label still names the
	// type) instead of letting it read as "no category".
	LongMemEvalUnknownCategory = 107
)

// longMemEvalCategories maps the dataset's question_type strings to the ids a
// report groups by.
var longMemEvalCategories = map[string]int{
	"multi-session":             LongMemEvalMultiSession,
	"temporal-reasoning":        LongMemEvalTemporalReasoning,
	"knowledge-update":          LongMemEvalKnowledgeUpdate,
	"single-session-user":       LongMemEvalSingleSessionUser,
	"single-session-assistant":  LongMemEvalSingleSessionAssistant,
	"single-session-preference": LongMemEvalSingleSessionPreference,
}

func lmeCategory(questionType string) int {
	if category, ok := longMemEvalCategories[strings.TrimSpace(questionType)]; ok {
		return category
	}
	return LongMemEvalUnknownCategory
}

// longMemEvalAbstentionSuffix marks the rows whose question the history cannot
// answer ("_abs" in question_id). Their gold is a statement that the
// information is missing, so grading them on containment would score every
// answer wrong.
const longMemEvalAbstentionSuffix = "_abs"

// longMemEvalDateTimeLayout is the shape of every haystack_dates and
// question_date value: "2023/05/30 (Tue) 23:40". It is not LoCoMo's layout
// ("1:56 pm on 8 May, 2023"), so the two parsers keep separate constants.
const longMemEvalDateTimeLayout = "2006/01/02 (Mon) 15:04"

func parseLongMemEvalDateTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(longMemEvalDateTimeLayout, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// LongMemEvalTurnID names one message of a LongMemEval session for the store.
// The dataset labels its gold at session granularity, so the harness derives a
// turn id from the session id and the message's position: ingest writes it
// under DatasetTurnMetadataKey like a LoCoMo dia_id, and grading reads it back
// to decide whether a session was recalled.
func LongMemEvalTurnID(sessionID string, position int) string {
	return sessionID + "#" + strconv.Itoa(position)
}

// LongMemEvalSessionOfTurn splits a turn id back into its session id. It is how
// grading turns "this message was recalled" into "its session was recalled".
func LongMemEvalSessionOfTurn(turnID string) (string, bool) {
	at := strings.LastIndex(turnID, "#")
	if at <= 0 || at == len(turnID)-1 {
		return "", false
	}
	return turnID[:at], true
}

// lmeRender says how one conversion writes a session date: into every message's
// text (the default, and what the LoCoMo conversion does), into every message's
// observed_at metadata, or nowhere. The text prefix and the anchor are
// alternative renderings of the same fact, which is why LoaderOptions refuses a
// conversion that would write both.
type lmeRender struct {
	dateless   bool
	observedAt bool
}

// lmeSession is one dataset session rendered for ingest: its identity, the date
// it happened, and the messages that carry both.
type lmeSession struct {
	id       string
	dateTime string
	messages []coremessage.Message
	ids      []string
	observed []string
}

// turn renders the session as one committed batch. The observation anchor is a
// property of the session -- every message exchanged in it was observed at its
// date -- so the anchor repeats across the messages, exactly as on the LoCoMo
// side.
func (session lmeSession) turn(questionID string) Turn {
	turn := Turn{
		IdempotencyKey: "longmemeval/" + questionID + "/" + session.id,
		Messages:       session.messages,
		DatasetIDs:     session.ids,
	}
	if hasNonEmpty(session.observed) {
		turn.ObservedAt = session.observed
	}
	return turn
}

// lmeSessions renders every session of one instance in date order. An
// unparsable date still renders its raw string into the text and leaves the
// metadata anchor empty rather than guessing one; the count is reported so
// anchor coverage is a reading on the run.
func lmeSessions(instance lmeRawInstance, render lmeRender, stats *LoaderStats) []lmeSession {
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
	sessions := make([]lmeSession, 0, len(indexes))
	for _, index := range indexes {
		sessionID := strconv.Itoa(index)
		if index < len(instance.HaystackSessionIDs) && strings.TrimSpace(instance.HaystackSessionIDs[index]) != "" {
			sessionID = strings.TrimSpace(instance.HaystackSessionIDs[index])
		}
		dateTime := lmeDate(instance, index)
		anchor := ""
		if render.observedAt && dateTime != "" {
			if parsed, ok := parseLongMemEvalDateTime(dateTime); ok {
				anchor = parsed.Format(time.RFC3339)
			} else {
				stats.ObservedAtUnparsed++
			}
		}
		rawTurns := instance.HaystackSessions[index]
		messages := make([]coremessage.Message, 0, len(rawTurns))
		ids := make([]string, 0, len(rawTurns))
		observed := make([]string, 0, len(rawTurns))
		for position, raw := range rawTurns {
			body := strings.TrimSpace(raw.Content)
			if body == "" {
				continue
			}
			role := coremessage.RoleUser
			if strings.EqualFold(raw.Role, "assistant") {
				role = coremessage.RoleAssistant
			}
			// The date is dataset content; carrying it in the text is the
			// conversion's choice, and the dateless conversion drops exactly
			// this step (the anchor above takes over).
			text := body
			if dateTime != "" && !render.dateless {
				text = "[" + dateTime + "] " + text
			}
			messages = append(messages, coremessage.NewTextMessage(role, text))
			ids = append(ids, LongMemEvalTurnID(sessionID, position))
			observed = append(observed, anchor)
			if anchor != "" {
				stats.ObservedAtAnchored++
			}
		}
		if len(messages) == 0 {
			continue
		}
		sessions = append(sessions, lmeSession{
			id: sessionID, dateTime: dateTime,
			messages: messages, ids: ids, observed: observed,
		})
	}
	return sessions
}

// lmeEvidenceSessions keeps the gold session ids the conversion actually
// rendered. A gold naming a session the haystack does not hold would make its
// question impossible to hit, so it is counted and dropped instead of grading
// as a permanent miss no retrieval could ever fix.
func lmeEvidenceSessions(answerSessionIDs []string, known map[string]struct{}, stats *LoaderStats) []string {
	evidence := make([]string, 0, len(answerSessionIDs))
	for _, sessionID := range uniqueNonEmpty(answerSessionIDs) {
		if _, ok := known[sessionID]; !ok {
			stats.EvidenceUnresolved++
			continue
		}
		evidence = append(evidence, sessionID)
	}
	if len(evidence) == 0 {
		return nil
	}
	return evidence
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
