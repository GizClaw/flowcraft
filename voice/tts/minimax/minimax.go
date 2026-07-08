package minimax

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	speechaudio "github.com/GizClaw/flowcraft/voice/audio"
	speechtts "github.com/GizClaw/flowcraft/voice/tts"
	"github.com/rs/xid"
)

// errConsumerGone signals that the output pipe's consumer stopped reading
// (out.Send returned false). It is distinguished from provider/decode errors
// so SynthesizeStream can Interrupt (nothing left to drain) instead of Close.
var errConsumerGone = errors.New("minimax tts stream: consumer gone")

const (
	defaultBaseURL = "https://api.minimaxi.com"
	defaultModel   = "speech-2.8-hd"
	defaultVoiceID = "male-qn-qingse"
)

func init() {
	speechtts.RegisterTTS("minimax", func(apiKey, baseURL string, opts ...speechtts.TTSProviderOption) (speechtts.TTS, error) {
		var ttsOpts []TTSOption
		if apiKey != "" {
			ttsOpts = append(ttsOpts, WithAPIKey(apiKey))
		}
		if baseURL != "" {
			ttsOpts = append(ttsOpts, WithBaseURL(baseURL))
		}
		for _, o := range opts {
			if opt, ok := o.(TTSOption); ok {
				ttsOpts = append(ttsOpts, opt)
			}
		}
		return New(ttsOpts...)
	})
}

// TTSOption configures a MiniMax TTS instance.
type TTSOption func(*TTS)

func (o TTSOption) ApplyTTSProvider(target any) {
	if t, ok := target.(*TTS); ok {
		o(t)
	}
}

func WithAPIKey(key string) TTSOption  { return func(t *TTS) { t.apiKey = key } }
func WithBaseURL(url string) TTSOption { return func(t *TTS) { t.baseURL = url } }
func WithModel(model string) TTSOption { return func(t *TTS) { t.model = model } }
func WithVoiceID(id string) TTSOption  { return func(t *TTS) { t.voiceID = id } }

// WithEmotion sets the default emotion on the TTS instance.
// For per-call control, use Emotion() instead.
func WithEmotion(e string) TTSOption { return func(t *TTS) { t.emotion = e } }

// ---------------------------------------------------------------------------
// Per-call TTSOptions (via Extra map)
// ---------------------------------------------------------------------------

const (
	KeyEmotion = "minimax.emotion"
	KeyPitch   = "minimax.pitch"
	KeyVol     = "minimax.vol"

	KeyLanguageBoost      = "minimax.language_boost"
	KeyNeedNoiseReduction = "minimax.need_noise_reduction"
	KeyNeedVolumeNorm     = "minimax.need_volume_normalization"
	KeyAIGCWatermark      = "minimax.aigc_watermark"
	KeyClonePromptAudio   = "minimax.clone_prompt_audio"
	KeyClonePromptText    = "minimax.clone_prompt_text"
	KeyClonePreviewText   = "minimax.clone_preview_text"
	KeyClonePreviewModel  = "minimax.clone_preview_model"
)

// Emotion returns a per-call TTSOption that overrides the instance-level emotion.
func Emotion(e string) speechtts.TTSOption { return speechtts.WithExtra(KeyEmotion, e) }

// Pitch returns a per-call TTSOption that sets pitch (-12 to 12).
func Pitch(p int) speechtts.TTSOption { return speechtts.WithExtra(KeyPitch, p) }

// Vol returns a per-call TTSOption that sets volume (0.1 to 10.0).
func Vol(v float64) speechtts.TTSOption { return speechtts.WithExtra(KeyVol, v) }

// TTS implements speechtts.TTS and speechtts.StreamTTS for the MiniMax T2A API.
type TTS struct {
	apiKey  string
	baseURL string
	model   string
	voiceID string
	emotion string
	client  *http.Client
}

func New(opts ...TTSOption) (*TTS, error) {
	t := &TTS{
		baseURL: defaultBaseURL,
		model:   defaultModel,
		voiceID: defaultVoiceID,
		client:  http.DefaultClient,
	}
	for _, o := range opts {
		o(t)
	}
	if t.apiKey == "" {
		return nil, fmt.Errorf("minimax tts: api key is required")
	}
	return t, nil
}

// --- request / response types ---

type t2aRequest struct {
	Model        string        `json:"model"`
	Text         string        `json:"text"`
	Stream       bool          `json:"stream"`
	VoiceSetting *voiceSetting `json:"voice_setting,omitempty"`
	AudioSetting *audioSetting `json:"audio_setting,omitempty"`
}

type voiceSetting struct {
	VoiceID string  `json:"voice_id"`
	Speed   float64 `json:"speed,omitempty"`
	Vol     float64 `json:"vol,omitempty"`
	Pitch   int     `json:"pitch,omitempty"`
	Emotion string  `json:"emotion,omitempty"`
}

type audioSetting struct {
	SampleRate int    `json:"sample_rate,omitempty"`
	Bitrate    int    `json:"bitrate,omitempty"`
	Format     string `json:"format,omitempty"`
	Channel    int    `json:"channel,omitempty"`
}

type t2aResponse struct {
	Data     *t2aData  `json:"data"`
	BaseResp *baseResp `json:"base_resp"`
	TraceID  string    `json:"trace_id"`
}

type t2aData struct {
	Audio  string `json:"audio"`
	Status int    `json:"status"`
}

type baseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// --- tts.TTS interface ---

func (t *TTS) Synthesize(ctx context.Context, text string, opts ...speechtts.TTSOption) (io.ReadCloser, error) {
	o := speechtts.ApplyTTSOptions(opts...)
	req := t.buildRequest(text, false, o)

	resp, err := t.doRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var result t2aResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("minimax tts: decode response: %w", err)
	}
	if result.BaseResp != nil && result.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("minimax tts: api error %d: %s", result.BaseResp.StatusCode, result.BaseResp.StatusMsg)
	}
	if result.Data == nil || result.Data.Audio == "" {
		return nil, fmt.Errorf("minimax tts: empty audio in response")
	}

	audioBytes, err := hex.DecodeString(result.Data.Audio)
	if err != nil {
		return nil, fmt.Errorf("minimax tts: decode hex audio: %w", err)
	}

	return io.NopCloser(bytes.NewReader(audioBytes)), nil
}

// --- get_voice request / response types ---

type getVoiceRequest struct {
	VoiceType string `json:"voice_type"`
}

type getVoiceResponse struct {
	SystemVoice     []systemVoiceInfo     `json:"system_voice"`
	VoiceCloning    []voiceCloningInfo    `json:"voice_cloning"`
	VoiceGeneration []voiceGenerationInfo `json:"voice_generation"`
	BaseResp        *baseResp             `json:"base_resp"`
}

type systemVoiceInfo struct {
	VoiceID     string   `json:"voice_id"`
	VoiceName   string   `json:"voice_name"`
	Description []string `json:"description"`
}

type voiceCloningInfo struct {
	VoiceID     string   `json:"voice_id"`
	Description []string `json:"description"`
	CreatedTime string   `json:"created_time"`
}

type voiceGenerationInfo struct {
	VoiceID     string   `json:"voice_id"`
	Description []string `json:"description"`
	CreatedTime string   `json:"created_time"`
}

func (t *TTS) Voices(ctx context.Context) ([]speechtts.Voice, error) {
	voices, err := t.fetchVoices(ctx)
	if err != nil {
		return fallbackVoices(), fmt.Errorf("minimax tts: using fallback voices: %w", err)
	}
	return voices, nil
}

func (t *TTS) fetchVoices(ctx context.Context) ([]speechtts.Voice, error) {
	body, err := json.Marshal(getVoiceRequest{VoiceType: "all"})
	if err != nil {
		return nil, err
	}

	url := strings.TrimRight(t.baseURL, "/") + "/v1/get_voice"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.apiKey)

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	var result getVoiceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.BaseResp != nil && result.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("api error %d: %s", result.BaseResp.StatusCode, result.BaseResp.StatusMsg)
	}

	var voices []speechtts.Voice
	for _, v := range result.SystemVoice {
		name := v.VoiceName
		if name == "" && len(v.Description) > 0 {
			name = v.Description[0]
		}
		voices = append(voices, speechtts.Voice{ID: v.VoiceID, Name: name})
	}
	for _, v := range result.VoiceCloning {
		name := v.VoiceID
		if len(v.Description) > 0 && v.Description[0] != "" {
			name = v.Description[0]
		}
		voices = append(voices, speechtts.Voice{ID: v.VoiceID, Name: name})
	}
	for _, v := range result.VoiceGeneration {
		name := v.VoiceID
		if len(v.Description) > 0 && v.Description[0] != "" {
			name = v.Description[0]
		}
		voices = append(voices, speechtts.Voice{ID: v.VoiceID, Name: name})
	}

	if len(voices) == 0 {
		return nil, fmt.Errorf("empty voice list")
	}
	return voices, nil
}

func fallbackVoices() []speechtts.Voice {
	return []speechtts.Voice{
		{ID: "male-qn-qingse", Name: "青涩青年音色", Lang: "zh"},
		{ID: "female-shaonv", Name: "少女音色", Lang: "zh"},
		{ID: "female-yujie", Name: "御姐音色", Lang: "zh"},
		{ID: "male-qn-jingying", Name: "精英青年音色", Lang: "zh"},
		{ID: "female-chengshu", Name: "成熟女声", Lang: "zh"},
		{ID: "male-qn-badao", Name: "霸道青年音色", Lang: "zh"},
		{ID: "English_Graceful_Lady", Name: "Graceful Lady", Lang: "en"},
		{ID: "English_Insightful_Speaker", Name: "Insightful Speaker", Lang: "en"},
		{ID: "English_radiant_girl", Name: "Radiant Girl", Lang: "en"},
		{ID: "English_Persuasive_Man", Name: "Persuasive Man", Lang: "en"},
		{ID: "Japanese_Whisper_Belle", Name: "Whisper Belle", Lang: "ja"},
	}
}

// --- voice.StreamTTS interface ---

func (t *TTS) SynthesizeStream(
	ctx context.Context,
	input speechaudio.Stream[string],
	opts ...speechtts.TTSOption,
) (speechaudio.Stream[speechtts.Utterance], error) {
	o := speechtts.ApplyTTSOptions(opts...)
	out := speechaudio.NewPipe[speechtts.Utterance](16)

	go func() {
		// done is closed when this goroutine returns so the watcher below
		// exits instead of leaking. The watcher only interrupts the output
		// when the caller's ctx is cancelled, leaving the normal EOF
		// (out.Close) and provider-error (out.Interrupt) paths untouched.
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				out.Interrupt()
			case <-done:
			}
		}()

		seq := 0
		for {
			// input.Read below ignores ctx, so bail out between sentences
			// once the caller cancels. Interrupt directly (the watcher may
			// race with done) to guarantee the consumer is unblocked.
			select {
			case <-ctx.Done():
				out.Interrupt()
				return
			default:
			}
			text, err := input.Read()
			if err == io.EOF {
				out.Close()
				return
			}
			if err != nil {
				out.Interrupt()
				return
			}
			if err := t.synthesizeStreamChunk(ctx, text, o, out, &seq); err != nil {
				if errors.Is(err, errConsumerGone) {
					// The consumer already went away, so there is no
					// buffered audio left to deliver — interrupt.
					out.Interrupt()
				} else {
					// Genuine provider/decode error mid-stream. Prefer Close
					// (normal EOF) over Interrupt so any already-synthesized
					// leading audio still buffered in the pipe drains to the
					// consumer instead of being discarded. The pipeline
					// (voice/pipeline.go runTTS) treats the stream's terminal
					// error identically to io.EOF — it stops reading without
					// surfacing the error as an event — so choosing Close
					// loses no error signal that callers rely on, and
					// FallbackTTS only falls back on stream-setup failure,
					// not on mid-stream errors.
					out.Close()
				}
				return
			}
		}
	}()

	return out, nil
}

func (t *TTS) synthesizeStreamChunk(
	ctx context.Context,
	text string,
	o *speechtts.TTSOptions,
	out *speechaudio.Pipe[speechtts.Utterance],
	seq *int,
) error {
	req := t.buildRequest(text, true, o)
	chunkID := xid.New().String()
	firstChunk := true

	resp, err := t.doRequest(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// Use a bufio.Reader (unbounded line length) instead of a bufio.Scanner:
	// the status=2 final SSE event carries the FULL cumulative hex-encoded
	// audio, which for longer utterances exceeds Scanner's max-token cap and
	// makes Scan() fail with bufio.ErrTooLong, failing the whole synthesis.
	// handleStreamLine skips that redundant final payload since all audio has
	// already been streamed incrementally via the status!=2 events.
	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadString('\n')
		if err := t.handleStreamLine(strings.TrimSpace(line), text, chunkID, o, out, seq, &firstChunk); err != nil {
			return err
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return fmt.Errorf("minimax tts stream: read response: %w", readErr)
		}
	}
}

// handleStreamLine parses one SSE line from the T2A streaming response and,
// when it carries an incremental audio chunk, sends it to out. It returns
// errConsumerGone if the consumer stopped reading, a provider error for API
// failures, or nil for lines that should be skipped (non-data lines, the
// redundant status=2 final event, or undecodable payloads).
func (t *TTS) handleStreamLine(
	line, text, chunkID string,
	o *speechtts.TTSOptions,
	out *speechaudio.Pipe[speechtts.Utterance],
	seq *int,
	firstChunk *bool,
) error {
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" {
		return nil
	}

	var respChunk t2aResponse
	if err := json.Unmarshal([]byte(data), &respChunk); err != nil {
		return nil
	}
	if respChunk.BaseResp != nil && respChunk.BaseResp.StatusCode != 0 {
		return fmt.Errorf("minimax tts stream: api error %d: %s",
			respChunk.BaseResp.StatusCode, respChunk.BaseResp.StatusMsg)
	}
	if respChunk.Data == nil || respChunk.Data.Audio == "" {
		return nil
	}
	// status=2 is the final event containing the full cumulative audio;
	// skip it because we already streamed all incremental chunks.
	if respChunk.Data.Status == 2 {
		return nil
	}

	audioBytes, err := hex.DecodeString(respChunk.Data.Audio)
	if err != nil {
		return nil
	}

	// Label the frame with the codec of the bytes MiniMax actually returned
	// (e.g. an opus request yields mp3 bytes, a wav request yields flac), not
	// the requested codec, so downstream decoders keyed off Format.Codec decode
	// the real audio rather than mis-decoding it.
	output := resolveOutput(o.Codec)

	// Only the first audio chunk of a sentence carries Text;
	// subsequent chunks for the same sentence leave Text empty
	// so downstream consumers can treat it as a sync-safe delta.
	uttText := ""
	if *firstChunk {
		uttText = text
		*firstChunk = false
	}

	outChunk := speechtts.Utterance{
		Frame: speechaudio.Frame{
			Data: audioBytes,
			Format: speechaudio.Format{
				Codec:      output.codec,
				SampleRate: effectiveSampleRate(o),
				Channels:   1,
				// BitDepth is left 0: MiniMax's mp3/flac output is a
				// compressed stream whose sample bit depth is only defined
				// after decoding, so we cannot state it here without guessing.
				BitDepth: 0,
			},
		},
		Text:     uttText,
		ChunkID:  chunkID,
		Sequence: *seq,
	}
	*seq++
	if !out.Send(outChunk) {
		return errConsumerGone
	}
	return nil
}

// --- helpers ---

// minimaxOutput describes the audio MiniMax actually returns for a requested
// codec: the format string sent in audio_setting.format and the codec of the
// bytes the API produces. MiniMax does not support every codec, so the
// requested codec and the produced bytes can differ. Keeping both fields
// together is the single source of truth so the format sent to the API and the
// Format.Codec label stamped on emitted frames can never diverge.
type minimaxOutput struct {
	format string
	codec  speechaudio.Codec
}

// resolveOutput maps a requested codec to the MiniMax API format string and the
// codec of the bytes the API actually returns for that format.
func resolveOutput(c speechaudio.Codec) minimaxOutput {
	switch c {
	case speechaudio.CodecMP3:
		return minimaxOutput{format: "mp3", codec: speechaudio.CodecMP3}
	case speechaudio.CodecOpus:
		// MiniMax does not support opus; it returns mp3 bytes.
		return minimaxOutput{format: "mp3", codec: speechaudio.CodecMP3}
	case speechaudio.CodecWAV:
		// MiniMax returns flac bytes for a "flac" format request.
		return minimaxOutput{format: "flac", codec: speechaudio.CodecFLAC}
	case speechaudio.CodecPCM:
		fallthrough
	default:
		// MiniMax has no raw PCM output; it returns mp3 bytes.
		return minimaxOutput{format: "mp3", codec: speechaudio.CodecMP3}
	}
}

// effectiveSampleRate returns the sample rate MiniMax is asked to encode at.
// It must match the value sent in audio_setting.sample_rate so the rate stamped
// on emitted frames describes the actual audio.
func effectiveSampleRate(o *speechtts.TTSOptions) int {
	if o.Rate <= 0 {
		return 32000
	}
	return o.Rate
}

func (t *TTS) buildRequest(text string, stream bool, o *speechtts.TTSOptions) *t2aRequest {
	voiceID := t.voiceID
	if o.Voice != "" {
		voiceID = o.Voice
	}

	speed := o.Speed
	if speed <= 0 {
		speed = 1.0
	}

	vs := &voiceSetting{
		VoiceID: voiceID,
		Speed:   speed,
		Vol:     o.ExtraFloat64(KeyVol, 1.0),
		Pitch:   o.ExtraInt(KeyPitch, 0),
		Emotion: o.ExtraString(KeyEmotion, t.emotion),
	}

	as := &audioSetting{
		SampleRate: effectiveSampleRate(o),
		Format:     resolveOutput(o.Codec).format,
		Channel:    1,
	}

	return &t2aRequest{
		Model:        t.model,
		Text:         text,
		Stream:       stream,
		VoiceSetting: vs,
		AudioSetting: as,
	}
}

func (t *TTS) doRequest(ctx context.Context, req *t2aRequest) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("minimax tts: marshal request: %w", err)
	}

	url := strings.TrimRight(t.baseURL, "/") + "/v1/t2a_v2"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("minimax tts: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.apiKey)

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("minimax tts: http request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("minimax tts: http %d: %s", resp.StatusCode, string(errBody))
	}

	return resp, nil
}

// ---------------------------------------------------------------------------
// Voice Cloning
// ---------------------------------------------------------------------------

// VoiceCloneOption configures a VoiceClone call.
type VoiceCloneOption func(*voiceCloneRequest)

func WithFileID(id int64) VoiceCloneOption {
	return func(r *voiceCloneRequest) { r.FileID = id }
}

func WithCloneVoiceID(id string) VoiceCloneOption {
	return func(r *voiceCloneRequest) { r.VoiceID = id }
}

func WithClonePrompt(audioFileID int64, text string) VoiceCloneOption {
	return func(r *voiceCloneRequest) {
		r.ClonePrompt = &clonePrompt{PromptAudio: audioFileID, PromptText: text}
	}
}

func WithPreviewText(text, model string) VoiceCloneOption {
	return func(r *voiceCloneRequest) {
		r.Text = text
		r.Model = model
	}
}

func WithLanguageBoost(lang string) VoiceCloneOption {
	return func(r *voiceCloneRequest) { r.LanguageBoost = lang }
}

func WithNoiseReduction(on bool) VoiceCloneOption {
	return func(r *voiceCloneRequest) { r.NeedNoiseReduction = &on }
}

func WithVolumeNormalization(on bool) VoiceCloneOption {
	return func(r *voiceCloneRequest) { r.NeedVolumeNormalization = &on }
}

func WithAIGCWatermark(on bool) VoiceCloneOption {
	return func(r *voiceCloneRequest) { r.AIGCWatermark = &on }
}

type voiceCloneRequest struct {
	FileID                  int64        `json:"file_id"`
	VoiceID                 string       `json:"voice_id"`
	ClonePrompt             *clonePrompt `json:"clone_prompt,omitempty"`
	Text                    string       `json:"text,omitempty"`
	Model                   string       `json:"model,omitempty"`
	LanguageBoost           string       `json:"language_boost,omitempty"`
	NeedNoiseReduction      *bool        `json:"need_noise_reduction,omitempty"`
	NeedVolumeNormalization *bool        `json:"need_volume_normalization,omitempty"`
	AIGCWatermark           *bool        `json:"aigc_watermark,omitempty"`
}

type clonePrompt struct {
	PromptAudio int64  `json:"prompt_audio"`
	PromptText  string `json:"prompt_text"`
}

// VoiceCloneResult holds the response from a voice clone request.
type VoiceCloneResult struct {
	VoiceID   string
	DemoAudio string
}

type voiceCloneResponse struct {
	DemoAudio      string `json:"demo_audio"`
	InputSensitive *struct {
		Type int `json:"type"`
	} `json:"input_sensitive"`
	BaseResp *baseResp `json:"base_resp"`
}

// VoiceClone clones a voice from a previously uploaded audio file.
// The returned VoiceCloneResult.VoiceID can be used with Synthesize / SynthesizeStream
// via speechtts.WithVoice(voiceID).
func (t *TTS) VoiceClone(ctx context.Context, fileID int64, voiceID string, opts ...VoiceCloneOption) (*VoiceCloneResult, error) {
	req := &voiceCloneRequest{
		FileID:  fileID,
		VoiceID: voiceID,
	}
	for _, o := range opts {
		o(req)
	}

	if req.FileID == 0 {
		return nil, fmt.Errorf("minimax voice_clone: file_id is required")
	}
	if req.VoiceID == "" {
		return nil, fmt.Errorf("minimax voice_clone: voice_id is required")
	}
	if req.Text != "" && req.Model == "" {
		return nil, fmt.Errorf("minimax voice_clone: model is required when text is provided")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("minimax voice_clone: marshal request: %w", err)
	}

	url := strings.TrimRight(t.baseURL, "/") + "/v1/voice_clone"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("minimax voice_clone: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.apiKey)

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("minimax voice_clone: http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("minimax voice_clone: http %d: %s", resp.StatusCode, string(errBody))
	}

	var result voiceCloneResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("minimax voice_clone: decode response: %w", err)
	}
	if result.BaseResp != nil && result.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("minimax voice_clone: api error %d: %s", result.BaseResp.StatusCode, result.BaseResp.StatusMsg)
	}

	return &VoiceCloneResult{
		VoiceID:   voiceID,
		DemoAudio: result.DemoAudio,
	}, nil
}

// Warmup implements speechtts.Warmer for connection pre-heating.
func (t *TTS) Warmup(ctx context.Context) error {
	url := strings.TrimRight(t.baseURL, "/") + "/v1/t2a_v2"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("minimax tts: warmup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("minimax tts: warmup: %w", err)
	}
	// Drain the body before closing so net/http's Transport can return the
	// connection to the idle pool for reuse — the whole point of warmup. A
	// bare Close on an unread body discards the connection instead.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// The TCP/TLS handshake — the expensive part warmup pre-heats — is complete
	// once we have any HTTP response. Posting "{}" to the T2A endpoint typically
	// yields HTTP 400, so only treat 5xx as a warmup failure; a client-side
	// (4xx) status still means the connection is established and pooled. A real
	// connection error would already have been returned above.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("minimax tts: warmup: HTTP %d", resp.StatusCode)
	}
	return nil
}
