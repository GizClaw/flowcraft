package dashscope

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// The file-transcription surface is the platform's asynchronous recognition
// pipeline, the one surface every non-realtime model family shares:
//
//	POST {base}/api/v1/services/audio/asr/transcription   (X-DashScope-Async: enable)
//	GET  {base}/api/v1/tasks/{task_id}                    (until terminal)
//	GET  {transcription_url}                              (the result file)
//
// The submit call names one file URL; the terminal query names the result
// file, whose transcripts carry the text, the sentence timing, and the word
// timing. That result link is the one artifact this driver downloads itself:
// the transcript is the answer, not a pointer at one, and the link is signed
// so the fetch carries no credential.
const transcribePath = "/api/v1/services/audio/asr/transcription"

// The parameters object always rides the request, even when empty: the
// Qwen-Audio filetrans family refuses a submitted task that omits it, and an
// empty object is valid on every other family.

// transcribeWire is one compiled transcription call: the file to recognize,
// and the parameters every control lowers to.
type transcribeWire struct {
	model          string
	fileURL        string
	languageHint   string
	context        string
	timestampAlign bool
	channelID      *int
	disfluency     *bool
	specialFilter  string
	vocabularyID   string
	vocabulary     map[string]int
	keepDialect    *bool
}

// transcribeResult is the result file behind transcription_url: the source
// properties, and one transcript per recognized channel.
type transcribeResult struct {
	FileURL    string `json:"file_url"`
	Properties struct {
		AudioFormat          string `json:"audio_format"`
		Channels             []int  `json:"channels"`
		OriginalSamplingRate int    `json:"original_sampling_rate"`
		// OriginalDurationMS is the source file's audio duration, which is
		// not the billed one: the platform bills the speech content each
		// track carries.
		OriginalDurationMS *int64 `json:"original_duration_in_milliseconds"`
	} `json:"properties"`
	Transcripts []transcribeTranscript `json:"transcripts"`
	// A result file normally carries transcripts; the error fields cover a
	// document that reports a failure in band instead.
	Code    string `json:"code"`
	Message string `json:"message"`
}

type transcribeTranscript struct {
	ChannelID         int                  `json:"channel_id"`
	ContentDurationMS int64                `json:"content_duration_in_milliseconds"`
	Text              string               `json:"text"`
	Sentences         []transcribeSentence `json:"sentences"`
}

type transcribeSentence struct {
	BeginTime int64            `json:"begin_time"`
	EndTime   int64            `json:"end_time"`
	Text      string           `json:"text"`
	Words     []transcribeWord `json:"words"`
}

type transcribeWord struct {
	BeginTime   int64  `json:"begin_time"`
	EndTime     int64  `json:"end_time"`
	Text        string `json:"text"`
	Punctuation string `json:"punctuation"`
}

// transcribeRaw is one executed transcription call: the result file, the
// language hint the request carried, the billed audio duration, and the
// request id of the terminal query.
type transcribeRaw struct {
	result       transcribeResult
	language     string
	billedMillis *int64
	requestID    string
}

// compileTranscribe lowers one canonical recognition request onto the task
// pipeline. The audio source has to be a URL — the platform fetches the file
// itself, so inline bytes have nowhere to ride — and every control lowers only
// where the declaration names it, because the platform documents the
// parameter set per model family.
func compileTranscribe(
	declared ModelSpec,
	name string,
) inference.Compiler[inference.TranscriptionRequest, transcribeWire] {
	params := declared.Transcribe
	return func(
		_ context.Context,
		_ model.ModelRef,
		request inference.TranscriptionRequest,
	) (inference.Compiled[transcribeWire], error) {
		ledger := inference.NewLedger(
			model.OperationTranscription,
			providerID,
			request.ActiveFields(),
		)
		wire := transcribeWire{model: wireModel(name, declared)}
		compileTranscribeAudio(&wire, request, ledger)
		if language := request.Language; language != "" {
			switch {
			case !params.LanguageHints:
				ledger.Reject(
					inference.FieldTranscriptionLanguage,
					"the declaration names no language control; set transcribe.language_hints to serve parameters.language_hints",
				)
			case len(params.Languages) > 0 &&
				!slices.Contains(params.Languages, language):
				ledger.Reject(
					inference.FieldTranscriptionLanguage,
					fmt.Sprintf(
						"the model accepts the languages %s, not %q",
						lists(params.Languages), language,
					),
				)
			default:
				wire.languageHint = language
			}
		}
		if prompt := request.Prompt; prompt != "" {
			switch {
			case !params.Context:
				ledger.Reject(
					inference.FieldTranscriptionPrompt,
					"the declaration names no context channel; set transcribe.context to lower the prompt onto input.context",
				)
			case params.PromptLimit > 0 &&
				len([]rune(prompt)) > params.PromptLimit:
				ledger.Reject(
					inference.FieldTranscriptionPrompt,
					fmt.Sprintf(
						"transcribe.prompt_limit caps the prompt at %d characters, and the platform truncates past its own cap rather than failing",
						params.PromptLimit,
					),
				)
			default:
				wire.context = prompt
			}
		}
		if request.Timestamps {
			if params.Timestamps {
				wire.timestampAlign = true
			} else {
				ledger.Reject(
					inference.FieldTranscriptionTimestamps,
					"the declaration names no timestamps control; set transcribe.timestamps to serve parameters.timestamp_alignment_enabled",
				)
			}
		}
		options, other := inference.ExtensionFor[TranscribeOptions](request.Extensions)
		ledger.RejectExtensions("file transcription", other)
		compileTranscribeOptions(&wire, params, options, ledger)
		if err := ledger.Err(); err != nil {
			return inference.Compiled[transcribeWire]{Report: ledger.Report()}, err
		}
		return inference.Compiled[transcribeWire]{
			Wire:   wire,
			Report: ledger.Report(),
		}, nil
	}
}

// compileTranscribeAudio lowers the canonical audio source onto
// input.file_urls.
func compileTranscribeAudio(
	wire *transcribeWire,
	request inference.TranscriptionRequest,
	ledger *inference.Ledger,
) {
	source := request.Audio
	if source.Kind() != media.SourceURL {
		ledger.Reject(
			inference.FieldTranscriptionAudio,
			"the file-transcription surface fetches its input by URL (input.file_urls); publish the audio and send a URL source",
		)
		return
	}
	value, err := transcribeFileURL(source.URL())
	if err != nil {
		ledger.Reject(
			inference.FieldTranscriptionAudio,
			"the platform could not fetch this audio URL: "+err.Error(),
		)
		return
	}
	wire.fileURL = value
}

// compileTranscribeOptions lowers TranscribeOptions onto the wire. A setting
// the declaration does not enable is rejected: the model's surface has no such
// parameter, so honoring it would silently change the request the caller
// believes they sent.
func compileTranscribeOptions(
	wire *transcribeWire,
	params TranscribeParams,
	options TranscribeOptions,
	ledger *inference.Ledger,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	if options.ChannelID != nil {
		if params.Channel {
			wire.channelID = options.ChannelID
		} else {
			ledger.Reject(
				field("channel_id"),
				"this model does not declare the audio track selector",
			)
		}
	}
	if options.DisfluencyRemoval != nil {
		if params.DisfluencyRemoval {
			wire.disfluency = options.DisfluencyRemoval
		} else {
			ledger.Reject(
				field("disfluency_removal_enabled"),
				"this model does not declare the filler-word filter",
			)
		}
	}
	if options.SpecialWordFilter != "" {
		if params.SpecialWordFilter {
			wire.specialFilter = options.SpecialWordFilter
		} else {
			ledger.Reject(
				field("special_word_filter"),
				"this model does not declare the sensitive-word filter",
			)
		}
	}
	if options.VocabularyID != "" {
		if params.Vocabulary {
			wire.vocabularyID = options.VocabularyID
		} else {
			ledger.Reject(
				field("vocabulary_id"),
				"this model does not declare the hotword channels",
			)
		}
	}
	if len(options.Vocabulary) > 0 {
		if params.Vocabulary {
			wire.vocabulary = maps.Clone(options.Vocabulary)
		} else {
			ledger.Reject(
				field("vocabulary"),
				"this model does not declare the hotword channels",
			)
		}
	}
	if options.KeepDialect != nil {
		if params.KeepDialect {
			wire.keepDialect = options.KeepDialect
		} else {
			ledger.Reject(
				field("keep_dialect"),
				"this model does not declare the dialect switch",
			)
		}
	}
}

// transcribeFileURL normalizes one file URL for input.file_urls. The platform
// downloads the file itself and asks callers to percent-encode paths that
// carry spaces or non-ASCII characters — without encoding an already encoded
// URL twice — so an http(s) URL is re-rendered through its parsed form, which
// escapes a raw path and leaves an escaped one alone. An oss:// locator is a
// signed OSS reference rather than a fetchable web URL and rides verbatim.
func transcribeFileURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the audio URL does not parse: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
		if parsed.Host == "" {
			return "", fmt.Errorf("the audio URL names no host")
		}
		return parsed.String(), nil
	case "oss":
		if parsed.Host == "" {
			return "", fmt.Errorf("the OSS URL names no bucket")
		}
		return raw, nil
	default:
		return "", fmt.Errorf(
			"unsupported scheme %q; the platform fetches http(s) URLs, or a signed oss:// locator",
			parsed.Scheme,
		)
	}
}

// transcribeRequest renders the wire body: the model, the file URL, and the
// parameters object carrying every control the caller asked for.
func transcribeRequest(wire transcribeWire) map[string]any {
	input := map[string]any{"file_urls": []string{wire.fileURL}}
	if wire.context != "" {
		// The prompt rides as one user turn of the recognition context: the
		// platform reads it as vocabulary and prior text that bias the
		// transcript.
		input["context"] = []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": wire.context,
			}},
		}}
	}
	parameters := map[string]any{}
	if wire.languageHint != "" {
		parameters["language_hints"] = []string{wire.languageHint}
	}
	if wire.channelID != nil {
		parameters["channel_id"] = []int{*wire.channelID}
	}
	if wire.timestampAlign {
		parameters["timestamp_alignment_enabled"] = true
	}
	if wire.disfluency != nil {
		parameters["disfluency_removal_enabled"] = *wire.disfluency
	}
	if wire.specialFilter != "" {
		parameters["special_word_filter"] = wire.specialFilter
	}
	if wire.vocabularyID != "" {
		parameters["vocabulary_id"] = wire.vocabularyID
	}
	if len(wire.vocabulary) > 0 {
		parameters["vocabulary"] = wire.vocabulary
	}
	if wire.keepDialect != nil {
		parameters["keep_dialect"] = *wire.keepDialect
	}
	return map[string]any{
		"model":      wire.model,
		"input":      input,
		"parameters": parameters,
	}
}

// transportTranscribe runs one recognition task end to end: submit, poll to a
// terminal state, then read the result file behind the link the terminal query
// names.
func transportTranscribe(
	client *mediaClient,
	pollInterval time.Duration,
) inference.Transport[transcribeWire, transcribeRaw] {
	return func(ctx context.Context, wire transcribeWire) (transcribeRaw, error) {
		fail := func(err error, requestID string) (transcribeRaw, error) {
			inference.LogProviderCall(
				ctx, providerID, "transcription", wire.model, err, requestID, "")
			return transcribeRaw{}, err
		}
		taskID, requestID, err := createTask(
			ctx, client, transcribePath, transcribeRequest(wire), "file transcription")
		if err != nil {
			return fail(err, "")
		}
		query, err := awaitTask(
			ctx, client, pollInterval, taskID, "file transcription")
		if err != nil {
			return fail(err, requestID)
		}
		requestID = nonEmpty(query.RequestID, requestID)
		link, err := transcriptionURL(query)
		if err != nil {
			return fail(err, requestID)
		}
		payload, err := client.getArtifact(ctx, link)
		if err != nil {
			return fail(err, requestID)
		}
		var result transcribeResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return fail(fmt.Errorf(
				"dashscope: file transcription result is not JSON: %w", err), requestID)
		}
		if result.Code != "" || result.Message != "" {
			return fail(errorBody{
				Code:    result.Code,
				Message: result.Message,
			}.failure("file transcription", http.StatusOK), requestID)
		}
		if len(result.Transcripts) == 0 {
			return fail(fmt.Errorf(
				"dashscope: file transcription result carries no transcripts"), requestID)
		}
		inference.LogProviderCall(
			ctx, providerID, "transcription", wire.model, nil, requestID, "")
		return transcribeRaw{
			result:       result,
			language:     wire.languageHint,
			billedMillis: billingMillis(query.Usage.Duration),
			requestID:    requestID,
		}, nil
	}
}

// decodeTranscribe lowers one result file into the canonical transcript. The
// platform's text is the transcript; its sentences become segments with their
// timing, and its words carry the trailing punctuation the platform keeps in a
// field of its own, so concatenating a segment's words reconstructs its text.
// Timing is decoded whenever the result reports it, whether or not the request
// asked for the alignment.
func decodeTranscribe() inference.Decoder[transcribeRaw, inference.TranscriptionResponse] {
	return func(
		_ context.Context,
		raw transcribeRaw,
	) (inference.TranscriptionResponse, error) {
		texts := make([]string, 0, len(raw.result.Transcripts))
		var segments []inference.TranscriptionSegment
		for _, transcript := range raw.result.Transcripts {
			if transcript.Text != "" {
				texts = append(texts, transcript.Text)
			}
			for _, sentence := range transcript.Sentences {
				if sentence.Text == "" {
					continue
				}
				segment := inference.TranscriptionSegment{
					Text:        sentence.Text,
					StartMillis: sentence.BeginTime,
					EndMillis:   sentence.EndTime,
				}
				for _, word := range sentence.Words {
					value := word.Text + word.Punctuation
					if value == "" {
						continue
					}
					segment.Words = append(segment.Words, inference.TranscriptionWord{
						Word:        value,
						StartMillis: word.BeginTime,
						EndMillis:   word.EndTime,
					})
				}
				segments = append(segments, segment)
			}
		}
		return inference.TranscriptionResponse{
			Text:           strings.Join(texts, "\n"),
			Segments:       segments,
			Language:       raw.language,
			DurationMillis: raw.result.Properties.OriginalDurationMS,
			Usage: inference.Usage{
				AudioDurationMillis: raw.billedMillis,
			},
			Metadata: inference.Metadata{RequestID: raw.requestID},
		}, nil
	}
}

// billingMillis converts the task's billed audio duration into the
// milliseconds the canonical usage carries. The platform reports the duration
// in seconds; a task that reported none reports none.
func billingMillis(seconds *float64) *int64 {
	if seconds == nil {
		return nil
	}
	millis := int64(math.Round(*seconds * 1000))
	return &millis
}

// openTranscribe binds the transcription compiler to the async file pipeline.
// The canonical operation also carries a duplex session shape, which rides the
// platform's WebSocket surface: this driver does not serve it, so the
// operations carry no session driver and a session request stays unsupported.
func openTranscribe(
	cls *clients,
	spec Spec,
	declared ModelSpec,
	id model.ModelID,
) (inference.TranscribeOperations, error) {
	unary, err := inference.BindTranscribe(
		compileTranscribe(declared, id.Name),
		transportTranscribe(cls.media, spec.pollInterval()),
		decodeTranscribe(),
	)
	if err != nil {
		return inference.TranscribeOperations{}, err
	}
	return inference.TranscribeOperations{Unary: unary}, nil
}
