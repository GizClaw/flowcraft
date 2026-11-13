package minimax

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// ProviderID is the identity this driver answers to.
const ProviderID = "minimax"

// DefaultBaseURL is the media API root used when the deployment does not
// override it: t2a speech synthesis and the H3 video task pipeline both hang
// off it. International deployments override BaseURL with
// https://api.minimax.io. The Messages surface is served by the anthropic
// driver, which owns its own endpoint.
//
// It is a host root: the driver appends the versioned path itself, because the
// two surfaces sit under different prefixes (/v1/t2a_v2 and
// /v2/video_generation). A base that already ends in /v1 or /v2 is rejected by
// [Settings.Validate] rather than silently producing /v1/v2/video_generation.
const DefaultBaseURL = "https://api.minimaxi.com"

// DefaultVideoPollInterval paces video task polling when the deployment does
// not override it.
const DefaultVideoPollInterval = 5 * time.Second

// modelKind selects the compiler and transport that serve a model. The kind,
// not the model name, picks the family.
type modelKind string

const (
	// kindTTS is the speech-synthesis family: t2a_v2, one JSON request, audio
	// back as hex — as one payload or as SSE chunks.
	kindTTS modelKind = "tts"
	// kindVideo is the video task family. Only the v2 task API
	// (MiniMax-H3: multimodal content, 768P/2K, 4-15s, ratio control) is
	// served; the flat v1 API the Hailuo 2.x/02 trio rides is not, because
	// its models are not declared by this driver.
	kindVideo modelKind = "video"
)

// Speech limits the platform documents. They are constants rather than
// declaration knobs because every model on the surface shares them.
const (
	// ttsSpeedMin and ttsSpeedMax bound the speaking-rate multiplier. A
	// canonical speed outside the band is rejected instead of clamped, so a
	// caller never believes it got a rate the endpoint did not apply.
	ttsSpeedMin = 0.5
	ttsSpeedMax = 2.0
	// ttsChannelMax is the widest stereo layout the surface serves.
	ttsChannelMax = 2
)

// ttsSampleRates are the sample rates t2a_v2 serves.
var ttsSampleRates = map[int]bool{
	8000: true, 16000: true, 22050: true, 24000: true, 32000: true, 44100: true,
}

// ttsEncodings are the payload encodings t2a_v2 serves.
var ttsEncodings = map[media.AudioEncoding]bool{
	media.AudioEncodingMP3:   true,
	media.AudioEncodingFLAC:  true,
	media.AudioEncodingOpus:  true,
	media.AudioEncodingPCM16: true,
}

// TTSParams declares the speech-surface facts for one kind "tts" model: what
// the endpoint produces when the request states nothing, and how much text it
// accepts.
type TTSParams struct {
	// PromptLimit caps the request text in characters; the platform's own cap
	// is per model. Zero leaves the length to the endpoint.
	PromptLimit int `json:"prompt_limit,omitempty"`
	// Encoding is the payload encoding the model serves when the request names
	// none. The empty value means the platform default, mp3.
	Encoding media.AudioEncoding `json:"encoding,omitempty"`
	// SampleRateHz and Channels declare the served stream's shape. A raw
	// encoding (pcm16) requires both, because the canonical audio format
	// requires them alongside it.
	SampleRateHz int `json:"sample_rate_hz,omitempty"`
	Channels     int `json:"channels,omitempty"`
}

// VideoParams declares the video-surface facts for one kind "video" model. Every
// leaf is optional: a fact the declaration omits keeps the platform envelope the
// driver defaults to, so an empty struct means "the documented H3 limits".
//
// The v2 API is the only video API this driver serves (MiniMax-H3), so these knobs
// narrow that surface for a model rather than selecting a different one.
type VideoParams struct {
	// PromptLimit caps the prompt in characters; zero keeps the platform's
	// documented 7000.
	PromptLimit int `json:"prompt_limit,omitempty"`
	// DurationMin and DurationMax bound the clip length in whole seconds; they
	// must stay inside the platform's 4-15 envelope, and an unset bound keeps
	// that edge. The default duration (6s) must remain inside the resolved
	// range.
	DurationMin *int `json:"duration_min_seconds,omitempty"`
	DurationMax *int `json:"duration_max_seconds,omitempty"`
	// Resolutions narrows the output tiers this model serves (768P/2K). An
	// unset list keeps both, and it must contain 768P because that is the
	// default tier.
	Resolutions []string `json:"resolutions,omitempty"`
	// Ratios narrows the shapes this model serves. An unset list keeps the
	// full set, and it must contain 16:9 because that is the text-to-video
	// default.
	Ratios []string `json:"ratios,omitempty"`
	// ReferenceImages, ReferenceVideos and ReferenceAudios bound the
	// multimodal reference inputs; zero keeps the platform maximum.
	ReferenceImages int `json:"reference_images,omitempty"`
	ReferenceVideos int `json:"reference_videos,omitempty"`
	ReferenceAudios int `json:"reference_audios,omitempty"`
}

// isZero reports whether the declaration narrows nothing. It replaces a struct
// comparison, which a slice-bearing struct does not support.
func (p VideoParams) isZero() bool {
	return p.PromptLimit == 0 && p.DurationMin == nil && p.DurationMax == nil &&
		len(p.Resolutions) == 0 && len(p.Ratios) == 0 &&
		p.ReferenceImages == 0 && p.ReferenceVideos == 0 && p.ReferenceAudios == 0
}

// Validate checks the video declaration for structural sanity. A narrowing that
// contradicts the platform envelope fails here rather than on every request.
func (p VideoParams) Validate() error {
	if p.PromptLimit < 0 {
		return fmt.Errorf("video.prompt_limit must not be negative")
	}
	min, max := videoDurationMin, videoDurationMax
	if p.DurationMin != nil {
		min = *p.DurationMin
	}
	if p.DurationMax != nil {
		max = *p.DurationMax
	}
	if min < videoDurationMin || min > videoDurationMax {
		return fmt.Errorf(
			"video.duration_min_seconds must be within [%d, %d], not %d",
			videoDurationMin, videoDurationMax, min)
	}
	if max < videoDurationMin || max > videoDurationMax {
		return fmt.Errorf(
			"video.duration_max_seconds must be within [%d, %d], not %d",
			videoDurationMin, videoDurationMax, max)
	}
	if min > max {
		return fmt.Errorf("video duration range is empty: %d > %d", min, max)
	}
	if videoDurationDefault < min || videoDurationDefault > max {
		return fmt.Errorf(
			"video duration range [%d, %d] excludes the %ds default",
			min, max, videoDurationDefault)
	}
	for _, resolution := range p.Resolutions {
		if !videoResolutions[resolution] {
			return fmt.Errorf(
				"video.resolutions holds %q, which is not a served tier (%s)",
				resolution, resolutionList())
		}
	}
	if len(p.Resolutions) > 0 {
		if !slices.Contains(p.Resolutions, videoResolutionDefault) {
			return fmt.Errorf(
				"video.resolutions must include %q, the default tier",
				videoResolutionDefault)
		}
	}
	for _, ratio := range p.Ratios {
		if !videoRatios[ratio] {
			return fmt.Errorf(
				"video.ratios holds %q, which is not a served ratio", ratio)
		}
	}
	if len(p.Ratios) > 0 {
		if !slices.Contains(p.Ratios, videoRatioTextDefault) {
			return fmt.Errorf(
				"video.ratios must include %q, the text-to-video default",
				videoRatioTextDefault)
		}
	}
	for _, bound := range []struct {
		name  string
		value int
		limit int
	}{
		{"reference_images", p.ReferenceImages, referenceImageMax},
		{"reference_videos", p.ReferenceVideos, referenceVideoMax},
		{"reference_audios", p.ReferenceAudios, referenceAudioMax},
	} {
		if bound.value < 0 || bound.value > bound.limit {
			return fmt.Errorf(
				"video.%s must be within [0, %d], not %d",
				bound.name, bound.limit, bound.value)
		}
	}
	return nil
}

// Settings is one deployment's configuration. Credentials are already
// resolved: the driver never reads a secret store.
type Settings struct {
	// BaseURL is the media API root (t2a and video). Defaults to
	// DefaultBaseURL.
	BaseURL string
	// APIKey authenticates every call.
	APIKey string
	// Headers adds request headers the deployment needs (a gateway's routing
	// header, for instance).
	Headers map[string]string
	// Timeout bounds one wire call. Defaults to ten minutes: a video create
	// call is answered immediately, but a large speech payload is not.
	Timeout time.Duration
	// Retries is the number of wire retries on transient failures. Zero
	// disables transport retries so the route Router owns the full budget.
	Retries int
	// HTTPClient overrides the transport. Nil builds one from the timeout.
	HTTPClient *http.Client
	// VideoPollInterval paces video task polling. A non-positive value keeps
	// DefaultVideoPollInterval; normalized() folds it before Validate runs, so
	// there is no negative value to reject later.
	VideoPollInterval time.Duration
	// Models is the deployment's model line-up. It is the only source of
	// models: the driver ships none, and nothing is inherited.
	Models []ModelSpec
}

// ModelSpec declares one model this deployment serves. The driver ships no
// line-up, so the declaration is complete: what it states is what the model
// promises, and a leaf it leaves out is undeclared rather than inherited.
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Capabilities declares what this model accepts and produces.
	Capabilities model.ModelCapabilities `json:"capabilities,omitempty"`
	// Limits declares the model's numeric capacity limits. Undeclared leaves
	// claim no bound rather than a zero one.
	Limits model.ModelLimits `json:"limits,omitempty"`
	// Lifecycle declares the model's discovery metadata: deprecation,
	// retirement time, and the model that replaces it. Empty means active.
	Lifecycle model.ModelLifecycle `json:"lifecycle,omitzero"`
	// WireModel overrides the model token sent to the API when the
	// deployment name is an alias. Empty sends the declared name.
	WireModel string `json:"wire_model,omitempty"`
	// TTS declares the speech-surface facts for a kind "tts" model.
	TTS TTSParams `json:"tts,omitempty"`
	// Video declares the video-surface facts for a kind "video" model.
	Video VideoParams `json:"video,omitempty"`
}

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Validate checks one model declaration.
func (m ModelSpec) Validate() error {
	if !modelNamePattern.MatchString(m.Name) {
		return fmt.Errorf("invalid model name %q", m.Name)
	}
	if m.WireModel != "" && !modelNamePattern.MatchString(m.WireModel) {
		return fmt.Errorf("invalid wire_model %q", m.WireModel)
	}
	if err := m.Capabilities.Validate(); err != nil {
		return err
	}
	if m.Capabilities.EmbeddingDimensions > 0 {
		return fmt.Errorf("minimax serves no embedding surface")
	}
	if m.Capabilities.Decision {
		return fmt.Errorf("minimax serves no decision surface")
	}
	switch modelKind(m.Kind) {
	case kindTTS:
		if !slices.Contains(m.Capabilities.Outputs, message.PartAudio) {
			return fmt.Errorf("tts family must declare audio output")
		}
		if !m.Video.isZero() {
			return fmt.Errorf("model declares video parameters but kind is %q", m.Kind)
		}
		if err := m.TTS.Validate(); err != nil {
			return err
		}
	case kindVideo:
		if !slices.Contains(m.Capabilities.Outputs, message.PartVideo) {
			return fmt.Errorf("video family must declare video output")
		}
		if m.TTS != (TTSParams{}) {
			return fmt.Errorf("model declares tts parameters but kind is %q", m.Kind)
		}
		if err := m.Video.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf(
			"unsupported kind %q; this driver serves %q and %q",
			m.Kind, kindTTS, kindVideo,
		)
	}
	return m.Limits.Validate()
}

// Validate checks the speech declaration for structural sanity.
func (p TTSParams) Validate() error {
	if p.PromptLimit < 0 {
		return fmt.Errorf("tts.prompt_limit must not be negative")
	}
	if p.SampleRateHz < 0 || p.Channels < 0 {
		return fmt.Errorf("tts.sample_rate_hz and tts.channels must not be negative")
	}
	if p.Encoding == "" {
		if p.SampleRateHz != 0 || p.Channels != 0 {
			return fmt.Errorf("tts sample rate and channels need an explicit tts.encoding")
		}
		return nil
	}
	if !ttsEncodings[p.Encoding] {
		return fmt.Errorf(
			"tts.encoding %q is not served; the platform encodes mp3, pcm16, flac, and opus",
			p.Encoding,
		)
	}
	format := media.AudioFormat{
		Encoding: p.Encoding, SampleRateHz: p.SampleRateHz, Channels: p.Channels,
	}
	if err := format.Validate(); err != nil {
		return fmt.Errorf("tts: %w", err)
	}
	if p.SampleRateHz != 0 && !ttsSampleRates[p.SampleRateHz] {
		return fmt.Errorf(
			"tts.sample_rate_hz %d is not served; the platform serves 8000/16000/22050/24000/32000/44100",
			p.SampleRateHz,
		)
	}
	if p.Channels > ttsChannelMax {
		return fmt.Errorf("tts.channels must be 1 or 2, not %d", p.Channels)
	}
	return nil
}

func (s Settings) normalized() Settings {
	if strings.TrimSpace(s.BaseURL) == "" {
		s.BaseURL = DefaultBaseURL
	}
	s.BaseURL = strings.TrimRight(s.BaseURL, "/")
	if s.Timeout <= 0 {
		s.Timeout = 10 * time.Minute
	}
	if s.VideoPollInterval <= 0 {
		s.VideoPollInterval = DefaultVideoPollInterval
	}
	return s
}

// Validate checks the settings that cannot be defaulted.
func (s Settings) Validate() error {
	if strings.TrimSpace(s.APIKey) == "" {
		return fmt.Errorf("minimax: api key is required")
	}
	if s.BaseURL != "" &&
		!strings.HasPrefix(s.BaseURL, "https://") &&
		!strings.HasPrefix(s.BaseURL, "http://") {
		return fmt.Errorf("minimax: base_url must be an http(s) URL")
	}
	if parsed, err := url.Parse(s.BaseURL); err == nil {
		path := strings.TrimRight(parsed.Path, "/")
		if strings.HasSuffix(path, "/v1") || strings.HasSuffix(path, "/v2") {
			return fmt.Errorf(
				"minimax: base_url must be a host root without a version segment; " +
					"/v1/t2a_v2 and /v2/video_generation are appended by the driver")
		}
	}
	if s.Retries < 0 {
		return fmt.Errorf("minimax: retries must not be negative")
	}
	seen := make(map[string]struct{}, len(s.Models))
	for _, declared := range s.Models {
		if err := declared.Validate(); err != nil {
			return fmt.Errorf("minimax: model %q: %w", declared.Name, err)
		}
		if _, exists := seen[declared.Name]; exists {
			return fmt.Errorf("minimax: duplicate model declaration %q", declared.Name)
		}
		seen[declared.Name] = struct{}{}
	}
	return nil
}

func descriptor(spec ModelSpec) model.ModelDescriptor {
	return model.ModelDescriptor{
		ID:           model.ModelID{Provider: ProviderID, Name: spec.Name},
		Capabilities: spec.Capabilities.Clone(),
		Limits:       spec.Limits.Clone(),
		Lifecycle:    spec.Lifecycle.Clone(),
	}
}

// wireModel returns the model token sent on the wire: the declared name unless
// the declaration aliases it to another model.
func wireModel(name string, declared ModelSpec) string {
	if declared.WireModel != "" {
		return declared.WireModel
	}
	return name
}
