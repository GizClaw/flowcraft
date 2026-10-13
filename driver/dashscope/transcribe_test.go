package dashscope

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
)

// ------------------------------------------------------------- compilation

func TestCompileTranscribeLowersControls(t *testing.T) {
	declared := testDeclaration(t, "paraformer-v2")
	request := inference.TranscriptionRequest{
		Audio:      audioURLSource(t, "https://cdn.example.com/会议 录音.wav"),
		Language:   "zh",
		Prompt:     "重庆 火锅 毛肚",
		Timestamps: true,
	}
	wire, report, err := compileTranscribeWire(t, declared, request)
	if err != nil {
		t.Fatalf("compile = %v", err)
	}
	if wire.model != "paraformer-v2" {
		t.Errorf("model = %q", wire.model)
	}
	// The platform fetches the file itself and asks callers to percent-encode
	// the path, so a raw URL is re-rendered through its parsed form: the path
	// survives and the space leaves the wire as %20.
	parsed, err := url.Parse(wire.fileURL)
	if err != nil {
		t.Fatalf("file url %q: %v", wire.fileURL, err)
	}
	if parsed.Path != "/会议 录音.wav" || strings.Contains(wire.fileURL, " ") {
		t.Errorf("file url = %q; want the raw path escaped once", wire.fileURL)
	}
	if wire.languageHint != "zh" {
		t.Errorf("language hint = %q", wire.languageHint)
	}
	if wire.context != "重庆 火锅 毛肚" {
		t.Errorf("context = %q", wire.context)
	}
	if !wire.timestampAlign {
		t.Error("timestamp alignment did not lower")
	}
	for _, field := range []inference.FieldID{
		inference.FieldTranscriptionAudio,
		inference.FieldTranscriptionLanguage,
		inference.FieldTranscriptionPrompt,
		inference.FieldTranscriptionTimestamps,
	} {
		decision, ok := decisionFor(report, field)
		if !ok || decision.Disposition != inference.Native {
			t.Errorf("%s decision = %+v", field, decision)
		}
	}

	body := transcribeRequest(wire)
	if body["model"] != "paraformer-v2" {
		t.Errorf("body model = %v", body["model"])
	}
	input, ok := body["input"].(map[string]any)
	if !ok {
		t.Fatalf("input = %#v", body["input"])
	}
	urls, ok := input["file_urls"].([]string)
	if !ok || len(urls) != 1 || urls[0] != wire.fileURL {
		t.Errorf("file_urls = %#v", input["file_urls"])
	}
	turns, ok := input["context"].([]map[string]any)
	if !ok || len(turns) != 1 || turns[0]["role"] != "user" {
		t.Fatalf("context = %#v", input["context"])
	}
	content, ok := turns[0]["content"].([]map[string]any)
	if !ok || len(content) != 1 ||
		content[0]["type"] != "input_text" ||
		content[0]["text"] != "重庆 火锅 毛肚" {
		t.Errorf("context content = %#v", turns[0]["content"])
	}
	parameters, ok := body["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters = %#v", body["parameters"])
	}
	hints, ok := parameters["language_hints"].([]string)
	if !ok || len(hints) != 1 || hints[0] != "zh" {
		t.Errorf("language_hints = %#v", parameters["language_hints"])
	}
	if parameters["timestamp_alignment_enabled"] != true {
		t.Errorf("timestamp_alignment_enabled = %#v",
			parameters["timestamp_alignment_enabled"])
	}
}

func TestCompileTranscribeRejectsUndeclaredControls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		declare func(ModelSpec) ModelSpec
		mutate  func(*testing.T, *inference.TranscriptionRequest)
		field   inference.FieldID
		reason  string
	}{
		{
			name: "language without a language control",
			declare: func(declared ModelSpec) ModelSpec {
				declared.Transcribe.LanguageHints = false
				return declared
			},
			mutate: func(_ *testing.T, request *inference.TranscriptionRequest) {
				request.Language = "zh"
			},
			field:  inference.FieldTranscriptionLanguage,
			reason: "names no language control",
		},
		{
			name:    "language outside the declared set",
			declare: func(declared ModelSpec) ModelSpec { return declared },
			mutate: func(_ *testing.T, request *inference.TranscriptionRequest) {
				request.Language = "fr"
			},
			field:  inference.FieldTranscriptionLanguage,
			reason: "accepts the languages zh, en",
		},
		{
			name: "prompt without a context channel",
			declare: func(declared ModelSpec) ModelSpec {
				declared.Transcribe.Context = false
				return declared
			},
			mutate: func(_ *testing.T, request *inference.TranscriptionRequest) {
				request.Prompt = "hotwords"
			},
			field:  inference.FieldTranscriptionPrompt,
			reason: "names no context channel",
		},
		{
			name:    "prompt past the declared cap",
			declare: func(declared ModelSpec) ModelSpec { return declared },
			mutate: func(_ *testing.T, request *inference.TranscriptionRequest) {
				request.Prompt = strings.Repeat("词", 401)
			},
			field:  inference.FieldTranscriptionPrompt,
			reason: "caps the prompt at 400 characters",
		},
		{
			name: "timestamps without a timestamps control",
			declare: func(declared ModelSpec) ModelSpec {
				declared.Transcribe.Timestamps = false
				return declared
			},
			mutate: func(_ *testing.T, request *inference.TranscriptionRequest) {
				request.Timestamps = true
			},
			field:  inference.FieldTranscriptionTimestamps,
			reason: "names no timestamps control",
		},
		{
			name:    "inline audio",
			declare: func(declared ModelSpec) ModelSpec { return declared },
			mutate: func(t *testing.T, request *inference.TranscriptionRequest) {
				request.Audio = audioBytesSource(t, []byte("RIFF0000"))
			},
			field:  inference.FieldTranscriptionAudio,
			reason: "publish the audio",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declared := tc.declare(testDeclaration(t, "paraformer-v2"))
			request := inference.TranscriptionRequest{
				Audio: audioURLSource(t, "https://cdn.example.com/a.wav"),
			}
			tc.mutate(t, &request)
			_, report, err := compileTranscribeWire(t, declared, request)
			if err == nil {
				t.Fatal("compile accepted an undeclared control")
			}
			if reason := rejectedReason(report, tc.field); !strings.Contains(reason, tc.reason) {
				t.Errorf("rejection = %q, want substring %q", reason, tc.reason)
			}
		})
	}
}

func TestCompileTranscribeOptions(t *testing.T) {
	declared := testDeclaration(t, "paraformer-v2")
	channel := 1
	disfluency := true
	request := inference.TranscriptionRequest{
		Audio: audioURLSource(t, "https://cdn.example.com/a.wav"),
		Extensions: inference.Extensions{
			TranscribeOptions{
				ChannelID:         &channel,
				DisfluencyRemoval: &disfluency,
				SpecialWordFilter: "[FILTER]敏感词",
				VocabularyID:      "vocab-1",
				Vocabulary:        map[string]int{"热词": 4},
			},
		},
	}
	wire, report, err := compileTranscribeWire(t, declared, request)
	if err != nil {
		t.Fatalf("compile = %v", err)
	}
	if wire.channelID == nil || *wire.channelID != 1 {
		t.Errorf("channel = %v", wire.channelID)
	}
	if wire.disfluency == nil || !*wire.disfluency {
		t.Errorf("disfluency = %v", wire.disfluency)
	}
	if wire.specialFilter != "[FILTER]敏感词" {
		t.Errorf("special word filter = %q", wire.specialFilter)
	}
	if wire.vocabularyID != "vocab-1" {
		t.Errorf("vocabulary id = %q", wire.vocabularyID)
	}
	if wire.vocabulary["热词"] != 4 {
		t.Errorf("vocabulary = %#v", wire.vocabulary)
	}
	for _, name := range []string{
		"channel_id", "disfluency_removal_enabled", "special_word_filter",
		"vocabulary_id", "vocabulary",
	} {
		field := transcribeOptionsField(name)
		decision, ok := decisionFor(report, field)
		if !ok || decision.Disposition != inference.Native {
			t.Errorf("%s decision = %+v", field, decision)
		}
	}

	body := transcribeRequest(wire)
	parameters, ok := body["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters = %#v", body["parameters"])
	}
	channels, ok := parameters["channel_id"].([]int)
	if !ok || len(channels) != 1 || channels[0] != 1 {
		t.Errorf("channel_id = %#v", parameters["channel_id"])
	}
	if parameters["disfluency_removal_enabled"] != true {
		t.Errorf("disfluency_removal_enabled = %#v",
			parameters["disfluency_removal_enabled"])
	}
	if parameters["special_word_filter"] != "[FILTER]敏感词" {
		t.Errorf("special_word_filter = %#v", parameters["special_word_filter"])
	}
	if parameters["vocabulary_id"] != "vocab-1" {
		t.Errorf("vocabulary_id = %#v", parameters["vocabulary_id"])
	}
	vocabulary, ok := parameters["vocabulary"].(map[string]int)
	if !ok || vocabulary["热词"] != 4 {
		t.Errorf("vocabulary = %#v", parameters["vocabulary"])
	}
}

func TestCompileTranscribeRejectsUndeclaredOptions(t *testing.T) {
	// fun-asr declares the hotword channels but neither the track selector,
	// the filler-word filter, nor the sensitive-word spec.
	channel := 0
	disfluency := false
	request := inference.TranscriptionRequest{
		Audio: audioURLSource(t, "https://cdn.example.com/a.wav"),
		Extensions: inference.Extensions{
			TranscribeOptions{
				ChannelID:         &channel,
				DisfluencyRemoval: &disfluency,
				SpecialWordFilter: "x",
			},
		},
	}
	_, report, err := compileTranscribeWire(t, testDeclaration(t, "fun-asr"), request)
	if err == nil {
		t.Fatal("compile accepted options the declaration leaves off")
	}
	for field, want := range map[string]string{
		"channel_id":                 "does not declare the audio track selector",
		"disfluency_removal_enabled": "does not declare the filler-word filter",
		"special_word_filter":        "does not declare the sensitive-word filter",
	} {
		if reason := rejectedReason(report, transcribeOptionsField(field)); !strings.Contains(reason, want) {
			t.Errorf("%s rejection = %q, want substring %q", field, reason, want)
		}
	}

	// keep_dialect belongs to the qwen-audio filetrans family alone; a
	// declaration that leaves it off rejects the switch rather than dropping
	// it silently.
	keep := true
	request = inference.TranscriptionRequest{
		Audio: audioURLSource(t, "https://cdn.example.com/a.wav"),
		Extensions: inference.Extensions{
			TranscribeOptions{KeepDialect: &keep},
		},
	}
	_, report, err = compileTranscribeWire(t, testDeclaration(t, "paraformer-v2"), request)
	if err == nil {
		t.Fatal("compile accepted the dialect switch")
	}
	if reason := rejectedReason(report, transcribeOptionsField("keep_dialect")); !strings.Contains(reason, "does not declare the dialect switch") {
		t.Errorf("keep_dialect rejection = %q", reason)
	}
}

func TestCompileTranscribeRejectsForeignExtension(t *testing.T) {
	request := inference.TranscriptionRequest{
		Audio: audioURLSource(t, "https://cdn.example.com/a.wav"),
		Extensions: inference.Extensions{
			ImageOptions{NegativePrompt: "logo"},
		},
	}
	_, report, err := compileTranscribeWire(t, testDeclaration(t, "paraformer-v2"), request)
	if err == nil {
		t.Fatal("compile accepted an image extension on a transcription request")
	}
	field := inference.ExtensionField("negative_prompt").Qualify(ImageOptions{})
	if reason := rejectedReason(report, field); !strings.Contains(reason, "does not apply to file transcription") {
		t.Errorf("rejection = %q", reason)
	}
}

func TestCompileTranscribeEmptyParametersRide(t *testing.T) {
	// The Qwen-Audio filetrans family refuses a submitted task whose
	// parameters object is missing, so the object rides every request even
	// when it is empty.
	wire, _, err := compileTranscribeWire(t, testDeclaration(t, "paraformer-v2"),
		inference.TranscriptionRequest{
			Audio: audioURLSource(t, "https://cdn.example.com/a.wav"),
		})
	if err != nil {
		t.Fatalf("compile = %v", err)
	}
	body := transcribeRequest(wire)
	parameters, ok := body["parameters"].(map[string]any)
	if !ok || len(parameters) != 0 {
		t.Errorf("parameters = %#v, want an empty object", body["parameters"])
	}
	input, ok := body["input"].(map[string]any)
	if !ok {
		t.Fatalf("input = %#v", body["input"])
	}
	if _, present := input["context"]; present {
		t.Error("a context-less request carried input.context")
	}
}

func TestCompileTranscribeWireModelAlias(t *testing.T) {
	declared := testDeclaration(t, "paraformer-v2")
	declared.WireModel = "paraformer-v2-2026-01-01"
	wire, _, err := compileTranscribeWire(t, declared, inference.TranscriptionRequest{
		Audio: audioURLSource(t, "https://cdn.example.com/a.wav"),
	})
	if err != nil {
		t.Fatalf("compile = %v", err)
	}
	if wire.model != "paraformer-v2-2026-01-01" {
		t.Errorf("model = %q, want the declared alias", wire.model)
	}
}

func TestTranscribeFileURL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		want   string
		reason string
	}{
		{
			name: "plain https",
			raw:  "https://cdn.example.com/a.wav",
			want: "https://cdn.example.com/a.wav",
		},
		{
			name: "escapes a raw path",
			raw:  "https://cdn.example.com/会议 录音.wav",
			want: "https://cdn.example.com/%E4%BC%9A%E8%AE%AE%20%E5%BD%95%E9%9F%B3.wav",
		},
		{
			name: "leaves an escaped url alone",
			raw:  "https://cdn.example.com/a%20b.wav",
			want: "https://cdn.example.com/a%20b.wav",
		},
		{
			name: "oss locator rides verbatim",
			raw:  "oss://bucket/key.wav?token=x",
			want: "oss://bucket/key.wav?token=x",
		},
		{
			name:   "no host",
			raw:    "https:///a.wav",
			reason: "names no host",
		},
		{
			name:   "oss without a bucket",
			raw:    "oss://",
			reason: "names no bucket",
		},
		{
			name:   "unsupported scheme",
			raw:    "ftp://cdn.example.com/a.wav",
			reason: "unsupported scheme",
		},
		{
			name:   "not a url",
			raw:    "://bad",
			reason: "does not parse",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := transcribeFileURL(tc.raw)
			if tc.reason != "" {
				if err == nil {
					t.Fatalf("transcribeFileURL(%q) = %q, want an error", tc.raw, value)
				}
				if !strings.Contains(err.Error(), tc.reason) {
					t.Errorf("error = %v, want substring %q", err, tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("transcribeFileURL(%q): %v", tc.raw, err)
			}
			if value != tc.want {
				t.Errorf("transcribeFileURL(%q) = %q, want %q", tc.raw, value, tc.want)
			}
		})
	}
}

func TestBillingMillis(t *testing.T) {
	if billed := billingMillis(nil); billed != nil {
		t.Errorf("billingMillis(nil) = %v", *billed)
	}
	for _, tc := range []struct {
		seconds float64
		want    int64
	}{
		{seconds: 3.512, want: 3512},
		{seconds: 5, want: 5000},
		{seconds: 9, want: 9000},
	} {
		billed := billingMillis(&tc.seconds)
		if billed == nil || *billed != tc.want {
			t.Errorf("billingMillis(%v) = %v, want %d", tc.seconds, billed, tc.want)
		}
	}
}

// ---------------------------------------------------------------- decoding

func TestDecodeTranscribeJoinsTranscripts(t *testing.T) {
	original := int64(3520)
	billed := int64(3512)
	raw := transcribeRaw{
		language:     "zh",
		requestID:    "req-asr-9",
		billedMillis: &billed,
		result: transcribeResult{
			Transcripts: []transcribeTranscript{
				{
					ChannelID:         0,
					ContentDurationMS: 3512,
					Text:              "今天天气不错。",
					Sentences: []transcribeSentence{
						{
							BeginTime: 0,
							EndTime:   3520,
							Text:      "今天天气不错。",
							Words: []transcribeWord{
								{BeginTime: 0, EndTime: 400, Text: "今"},
								{BeginTime: 400, EndTime: 800, Text: "天"},
								{BeginTime: 800, EndTime: 3520, Text: "天气不错", Punctuation: "。"},
								{BeginTime: 3520, EndTime: 3520},
							},
						},
						// A sentence with no text has nothing to commit.
						{BeginTime: 3520, EndTime: 3520},
					},
				},
				{ChannelID: 1, Text: "第二段。"},
			},
		},
	}
	raw.result.Properties.OriginalDurationMS = &original

	response, err := decodeTranscribe()(context.Background(), raw)
	if err != nil {
		t.Fatalf("decode = %v", err)
	}
	if response.Text != "今天天气不错。\n第二段。" {
		t.Errorf("text = %q", response.Text)
	}
	if len(response.Segments) != 1 {
		t.Fatalf("segments = %#v", response.Segments)
	}
	segment := response.Segments[0]
	if segment.Text != "今天天气不错。" ||
		segment.StartMillis != 0 || segment.EndMillis != 3520 {
		t.Errorf("segment = %+v", segment)
	}
	if len(segment.Words) != 3 {
		t.Fatalf("words = %#v", segment.Words)
	}
	// The punctuation rides its own field, so concatenating a segment's
	// words has to reconstruct the sentence text.
	joined := ""
	for _, word := range segment.Words {
		joined += word.Word
	}
	if joined != segment.Text {
		t.Errorf("words join to %q, want %q", joined, segment.Text)
	}
	if response.Language != "zh" {
		t.Errorf("language = %q", response.Language)
	}
	if response.DurationMillis == nil || *response.DurationMillis != 3520 {
		t.Errorf("duration = %v", response.DurationMillis)
	}
	if response.Usage.AudioDurationMillis == nil ||
		*response.Usage.AudioDurationMillis != 3512 {
		t.Errorf("billed duration = %v", response.Usage.AudioDurationMillis)
	}
	if response.Metadata.RequestID != "req-asr-9" {
		t.Errorf("request id = %q", response.Metadata.RequestID)
	}
}

// -------------------------------------------------------------- transport

// transcribeResultFile is the result document the transcription task's link
// serves: the source properties plus one transcript per channel.
const transcribeResultFile = `{"file_url":"https://cdn.example.com/audio.wav",` +
	`"properties":{"audio_format":"wav","channels":[0],` +
	`"original_sampling_rate":16000,"original_duration_in_milliseconds":3520},` +
	`"transcripts":[{"channel_id":0,"content_duration_in_milliseconds":3512,` +
	`"text":"今天天气不错。","sentences":[{"begin_time":0,"end_time":3520,` +
	`"text":"今天天气不错。","words":[` +
	`{"begin_time":0,"end_time":400,"text":"今","punctuation":""},` +
	`{"begin_time":400,"end_time":800,"text":"天","punctuation":""},` +
	`{"begin_time":800,"end_time":3520,"text":"天气不错","punctuation":"。"}]}]}]}`

func TestTransportTranscribeTask(t *testing.T) {
	platform := &fakePlatform{}
	platform.create = func(body map[string]any) (int, string) {
		if body["model"] != "paraformer-v2" {
			t.Errorf("create model = %v", body["model"])
		}
		return http.StatusOK,
			`{"output":{"task_id":"task-asr-1","task_status":"PENDING"},"request_id":"req-asr-1"}`
	}
	platform.query = func(taskID string) (int, string) {
		if taskID != "task-asr-1" {
			t.Errorf("query task id = %q", taskID)
		}
		return http.StatusOK,
			`{"output":{"task_id":"task-asr-1","task_status":"SUCCEEDED","results":[` +
				`{"transcription_url":"` + platform.base + `/artifacts/result.json",` +
				`"subtask_status":"SUCCEEDED"}]},` +
				`"usage":{"duration":3.512},"request_id":"req-asr-2"}`
	}
	platform.artifact = func(path string) (int, string) {
		if path != "/artifacts/result.json" {
			t.Errorf("artifact path = %q", path)
		}
		return http.StatusOK, transcribeResultFile
	}
	client := testClients(t, platform)

	raw, err := transportTranscribe(client, time.Millisecond)(
		context.Background(),
		transcribeWire{
			model:        "paraformer-v2",
			fileURL:      "https://cdn.example.com/audio.wav",
			languageHint: "zh",
		},
	)
	if err != nil {
		t.Fatalf("transport = %v", err)
	}
	if raw.requestID != "req-asr-2" {
		t.Errorf("request id = %q", raw.requestID)
	}
	if raw.language != "zh" {
		t.Errorf("language = %q", raw.language)
	}
	if raw.billedMillis == nil || *raw.billedMillis != 3512 {
		t.Errorf("billed millis = %v", raw.billedMillis)
	}

	calls := platform.recorded()
	if len(calls) != 3 {
		t.Fatalf("the platform saw %d calls: %+v", len(calls), calls)
	}
	create := calls[0]
	if create.method != http.MethodPost || create.path != transcribePath {
		t.Errorf("create call = %s %s", create.method, create.path)
	}
	if create.async != "enable" {
		t.Errorf("create call carries %s = %q", headerAsync, create.async)
	}
	if !strings.Contains(create.auth, "sk-test") {
		t.Errorf("create call auth = %q", create.auth)
	}
	input, ok := create.body["input"].(map[string]any)
	if !ok {
		t.Fatalf("create body input = %#v", create.body["input"])
	}
	urls, ok := input["file_urls"].([]any)
	if !ok || len(urls) != 1 || urls[0] != "https://cdn.example.com/audio.wav" {
		t.Errorf("file_urls = %#v", input["file_urls"])
	}
	parameters, ok := create.body["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("create body parameters = %#v", create.body["parameters"])
	}
	hints, ok := parameters["language_hints"].([]any)
	if !ok || len(hints) != 1 || hints[0] != "zh" {
		t.Errorf("language_hints = %#v", parameters["language_hints"])
	}
	query := calls[1]
	if query.method != http.MethodGet || query.path != taskPathPrefix+"task-asr-1" {
		t.Errorf("query call = %s %s", query.method, query.path)
	}
	// The result link is signed and lives on the platform's result host, so
	// the download the driver performs itself carries no credential.
	artifact := calls[2]
	if artifact.method != http.MethodGet || artifact.path != "/artifacts/result.json" {
		t.Errorf("artifact call = %s %s", artifact.method, artifact.path)
	}
	if artifact.auth != "" {
		t.Errorf("artifact call carried a credential: %q", artifact.auth)
	}

	response, err := decodeTranscribe()(context.Background(), raw)
	if err != nil {
		t.Fatalf("decode = %v", err)
	}
	if response.Text != "今天天气不错。" {
		t.Errorf("text = %q", response.Text)
	}
	if response.Metadata.RequestID != "req-asr-2" {
		t.Errorf("decoded request id = %q", response.Metadata.RequestID)
	}
}

func TestTranscribeURLReadsResultLink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		want   string
		reason string
		failed bool
	}{
		{
			name:   "no results",
			query:  `{"output":{"task_id":"t","task_status":"SUCCEEDED"}}`,
			reason: "returned no results",
		},
		{
			name: "a failed subtask",
			query: `{"output":{"results":[{"subtask_status":"FAILED",` +
				`"code":"InvalidFile.DownloadFailed",` +
				`"message":"the file could not be fetched"}]}}`,
			reason: "the file could not be fetched",
			failed: true,
		},
		{
			name:   "no link",
			query:  `{"output":{"results":[{"subtask_status":"RUNNING"}]}}`,
			reason: "returned no result link",
		},
		{
			name: "the link",
			query: `{"output":{"results":[{"subtask_status":"SUCCEEDED",` +
				`"transcription_url":"https://cdn.example.com/r.json"}]}}`,
			want: "https://cdn.example.com/r.json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, err := transcriptionURL(taskQueryFrom(t, tc.query))
			if tc.reason != "" {
				if err == nil {
					t.Fatalf("transcriptionURL = %q, want an error", link)
				}
				if !strings.Contains(err.Error(), tc.reason) {
					t.Errorf("error = %v, want substring %q", err, tc.reason)
				}
				if tc.failed && !errdefs.IsValidation(err) {
					t.Errorf("error = %v, want validation: a failed subtask classified by its code", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("transcriptionURL: %v", err)
			}
			if link != tc.want {
				t.Errorf("link = %q, want %q", link, tc.want)
			}
		})
	}
}

func TestTransportTranscribeResultFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		reason string
	}{
		{
			name: "in-band failure envelope",
			body: `{"code":"ASR_FILE_DOWNLOAD_FAILED",` +
				`"message":"the file could not be fetched"}`,
			reason: "ASR_FILE_DOWNLOAD_FAILED",
		},
		{
			name: "no transcripts",
			body: `{"file_url":"https://cdn.example.com/audio.wav",` +
				`"properties":{}}`,
			reason: "carries no transcripts",
		},
		{
			name:   "not json",
			body:   "not json",
			reason: "is not JSON",
		},
		{
			name:   "artifact http error",
			status: http.StatusInternalServerError,
			body:   `{"code":"InternalError","message":"boom"}`,
			reason: "HTTP 500",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform := &fakePlatform{}
			platform.create = func(map[string]any) (int, string) {
				return http.StatusOK,
					`{"output":{"task_id":"task-1","task_status":"PENDING"},"request_id":"req-1"}`
			}
			platform.query = func(string) (int, string) {
				return http.StatusOK,
					`{"output":{"task_status":"SUCCEEDED","results":[` +
						`{"subtask_status":"SUCCEEDED","transcription_url":"` +
						platform.base + `/artifacts/r.json"}]},"request_id":"req-2"}`
			}
			platform.artifact = func(string) (int, string) {
				if tc.status == 0 {
					return http.StatusOK, tc.body
				}
				return tc.status, tc.body
			}
			client := testClients(t, platform)
			_, err := transportTranscribe(client, time.Millisecond)(
				context.Background(),
				transcribeWire{
					model:   "paraformer-v2",
					fileURL: "https://cdn.example.com/audio.wav",
				},
			)
			if err == nil {
				t.Fatal("transport accepted a failing result file")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error = %v, want substring %q", err, tc.reason)
			}
		})
	}
}

// taskQueryFrom decodes a task-envelope fixture into the query shape the
// transport reads, the way the platform's own bytes arrive.
func taskQueryFrom(t *testing.T, raw string) taskQuery {
	t.Helper()
	var query taskQuery
	if err := json.Unmarshal([]byte(raw), &query); err != nil {
		t.Fatalf("task query %q: %v", raw, err)
	}
	return query
}
