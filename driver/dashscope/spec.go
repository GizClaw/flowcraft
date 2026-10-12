package dashscope

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/resource"
)

// SecretAPIKey is the provider-owned secret name for the DashScope API key.
const SecretAPIKey = "api_key"

// DefaultBaseURL is the DashScope API host used when the deployment does not
// override base_url. Regional deployments point base_url at their workspace
// host instead, e.g. https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com (or
// https://dashscope-intl.aliyuncs.com for Singapore); keys and hosts are
// region-bound, so a mismatched pair fails authentication rather than
// silently degrading.
const DefaultBaseURL = "https://dashscope.aliyuncs.com"

// defaultPollInterval paces task polling when the spec does not override it.
// The platform's media tasks take one to five minutes, so a few seconds of
// polling adds negligible load while keeping latency low.
const defaultPollInterval = 5 * time.Second

// Spec is the provider-level configuration surface. It is credential-free:
// secrets resolve per profile and never appear here.
type Spec struct {
	// BaseURL is the DashScope API host (no /api/v1 suffix; paths are
	// appended). Defaults to DefaultBaseURL.
	BaseURL string `json:"base_url,omitempty"`
	// WorkspaceID rides every request as X-DashScope-WorkSpace. It is
	// required for RAM sub-account keys and optional for primary-account
	// keys, so leaving it empty sends no header.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// HTTPRetries bounds wire-level HTTP retries. Zero disables transport
	// retries so the route Router owns the full retry budget; nil keeps
	// the httpkit default. Task polling is not a retry: it is modeled as
	// the wait it is.
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// PollIntervalMillis paces task polling; defaults to 5000. Declared
	// values below one millisecond keep the default rather than spinning.
	PollIntervalMillis resource.Int `json:"poll_interval_millis,omitempty"`
	// Models declares the line-up this deployment serves. It is the only
	// source of models: the driver ships none, and nothing is inherited.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one model this deployment serves. The driver ships no
// line-up, so the declaration is complete: what it states is what the model
// promises, and a leaf it leaves out is undeclared rather than inherited.
// The facts no capability kind expresses — which protocol and endpoint
// family the model speaks, its size and duration envelope, its optional
// knobs — are declared here.
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
	// deployment name is an alias, e.g. a dated snapshot
	// (wan2.7-t2v-2026-06-12) addressed under a stable name. Empty sends
	// the declared name.
	WireModel string `json:"wire_model,omitempty"`
	// Image declares the image-surface facts for a kind "image" model.
	Image ImageParams `json:"image,omitempty"`
	// Video declares the video-surface facts for a kind "video" model.
	Video VideoParams `json:"video,omitempty"`
}

// ImageParams declares the image-surface facts for a kind "image" model.
// Every leaf is optional: a fact the declaration omits stays unvalidated and
// the endpoint's own validation decides.
type ImageParams struct {
	// API selects the protocol: "" (the async task pipeline) or "sync" (the
	// single multimodal-generation call Wan 2.6 and later image models
	// serve).
	API string `json:"api,omitempty"`
	// Edit marks image-editing models. They ride the image2image endpoint,
	// require the edit function option, and read base_image_url — plus
	// mask_image_url for the masked functions — from input images.
	Edit bool `json:"edit,omitempty"`
	// ReferenceImage accepts an input image as input.ref_img: the wanx-v1
	// reference-image (垫图) mode. Models without it reject reference
	// images, which is the honest reading — the text2image surface of most
	// models has no channel for them.
	ReferenceImage bool `json:"reference_image,omitempty"`
	// Style accepts parameters.style (the wanx-v1 style enum).
	Style bool `json:"style,omitempty"`
	// PromptLimit caps the prompt in characters (wanx-v1 800, wan2.1/2.2
	// 500, wan2.5/2.6 2000, wan2.7 2100). Zero leaves it uncapped.
	PromptLimit int `json:"prompt_limit,omitempty"`
	// CountMax caps parameters.n. Zero leaves it uncapped. The driver sends
	// 1 unless the intent asks for more — the platform default of 4 bills
	// four images.
	CountMax int `json:"count_max,omitempty"`
	// Sizes enumerates the accepted 宽*高 sizes, for families with a fixed
	// list (wanx-v1).
	Sizes []string `json:"sizes,omitempty"`
	// MinEdge/MaxEdge bound each side of the requested size
	// (the wan2.1/wan2.2 envelope: 512..1440).
	MinEdge int `json:"min_edge,omitempty"`
	MaxEdge int `json:"max_edge,omitempty"`
	// MinPixels/MaxPixels bound total pixels (the wan2.5/2.6 envelope:
	// 1280*1280..1440*1440).
	MinPixels int64 `json:"min_pixels,omitempty"`
	MaxPixels int64 `json:"max_pixels,omitempty"`
	// Format is the pixel format every delivery carries; "" means png, the
	// family default. A requested output format the declaration does not
	// name rejects rather than mislabeling the bytes.
	Format media.ImageFormat `json:"format,omitempty"`
}

// VideoParams declares the video-surface facts for a kind "video" model.
// Like ImageParams, every leaf is optional and an omitted fact stays
// unvalidated.
type VideoParams struct {
	// Input selects the endpoint family and the required input shape:
	// "t2v" (default — a prompt alone), "i2v" (prompt plus one image,
	// input.img_url), or "kf2v" (first_frame_url, with an optional
	// last_frame_url, on the image2video endpoint).
	Input string `json:"input,omitempty"`
	// SizeParam selects how the resolution rides the wire: "" or
	// "resolution" sends the tier token (480P/720P/1080P), "size" sends the
	// concrete 宽*高 that Sizes resolves (the legacy-protocol Wan 2.5/2.6
	// text-to-video surface).
	SizeParam string `json:"size_param,omitempty"`
	// Resolutions lists the resolution tiers the model accepts, e.g.
	// [480P 720P 1080P].
	Resolutions []string `json:"resolutions,omitempty"`
	// Ratios lists the aspect ratios the model accepts (Wan 2.7's ratio
	// knob).
	Ratios []string `json:"ratios,omitempty"`
	// Sizes maps a tier and ratio to the concrete size a size-param model
	// wants: {"720P": {"16:9": "1280*720"}}. It is the lookup the compiler
	// uses to turn a canonical tier into the concrete resolution the wire
	// requires, and it doubles as the accepted-size declaration.
	Sizes map[string]map[string]string `json:"sizes,omitempty"`
	// Durations lists the whole-second durations the model accepts; empty
	// falls back to MinDuration/MaxDuration.
	Durations []int `json:"durations,omitempty"`
	// MinDuration/MaxDuration bound a duration range in seconds (Wan 2.6+
	// accept 2..15; several 2.2-era models fix 5).
	MinDuration int `json:"min_duration,omitempty"`
	MaxDuration int `json:"max_duration,omitempty"`
	// Audio accepts an input audio part, sent as input.audio_url.
	Audio bool `json:"audio,omitempty"`
	// AudioSwitch accepts parameters.audio, the output-audio switch on
	// models that can synthesize a soundtrack.
	AudioSwitch bool `json:"audio_switch,omitempty"`
	// ShotType accepts parameters.shot_type (single or multi shot).
	ShotType bool `json:"shot_type,omitempty"`
	// Template accepts the video-effect selector (input.template).
	Template bool `json:"template,omitempty"`
	// PromptLimit caps the prompt in characters (wan2.2/2.1 800, wan2.5/2.6
	// 1500, wan2.7 5000). Zero leaves it uncapped.
	PromptLimit int `json:"prompt_limit,omitempty"`
	// Media switches the surface to the media array the Wan 3.0 all-in-one
	// models take their input through, where the image, video, audio, and
	// file parts ride input.media as typed entries instead of the flat keys
	// above. The array carries its own input shape, so a media declaration
	// leaves input empty.
	Media *MediaParams `json:"media,omitempty"`
	// NegativePrompt accepts the video_options negative prompt, which the
	// Wan 2.x surfaces document as input.negative_prompt. The Wan 3.0 media
	// array does not carry one, so its declarations leave the knob off and
	// the compiler rejects the option instead of sending a field the
	// endpoint does not document.
	NegativePrompt bool `json:"negative_prompt,omitempty"`
	// SmartDuration accepts the smart-duration request (parameters.duration
	// -1), on which the model picks the length from the prompt and the
	// media it was given. Only the Wan 3.0 media array documents it.
	SmartDuration bool `json:"smart_duration,omitempty"`
}

// MediaParams declares the channels of the media array the Wan 3.0
// all-in-one models take their input through: one video-synthesis endpoint
// whose input.media carries typed assets instead of the flat 2.x keys. The
// array order numbers each type for the prompt ("图1", "视频1", "音频1"), so
// the compiler appends entries in request order.
//
// The platform refuses first/last frames together with any other asset, and
// a lone input image would have no single home — the same rule that keeps
// reference_image and edit apart on the image surface — so a declaration
// names one mode: the frames pair, or reference channels plus the document
// channels. Declare the same wire model twice to serve both.
type MediaParams struct {
	// FirstFrame/LastFrame accept image parts as first_frame and
	// last_frame entries. LastFrame needs FirstFrame: a last frame has no
	// first to end.
	FirstFrame bool `json:"first_frame,omitempty"`
	LastFrame  bool `json:"last_frame,omitempty"`
	// ReferenceImages/ReferenceVideos/ReferenceAudios cap the entries of
	// that type one request may carry; wan3.0 accepts 10 images, 5 videos,
	// and 5 audios, the videos and audios totalling at most 15 seconds
	// each. Zero leaves the channel unaccepted, and the platform sums the
	// durations, so only the counts are checked here.
	ReferenceImages int `json:"reference_images,omitempty"`
	ReferenceVideos int `json:"reference_videos,omitempty"`
	ReferenceAudios int `json:"reference_audios,omitempty"`
	// File accepts one file part as a file entry and Link one public web
	// page, which the caller passes through video_options.link; the two are
	// mutually exclusive per request. A request carrying either one has to
	// leave the platform's prompt rewrite on.
	File bool `json:"file,omitempty"`
	Link bool `json:"link,omitempty"`
}

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var workspaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// resolutionPattern pins a resolution tier to the canonical shape the
// platform documents: a digit count followed by P (720P) or K (4K).
var resolutionPattern = regexp.MustCompile(`^[0-9]+[PK]$`)

// Validate checks the model declaration for structural sanity: the kind is
// one this driver serves, the declaration facts land on the right kind, and
// every declared fact is well-formed. It deliberately does not check
// capability coverage — the family contract in validateModel owns that.
func (m ModelSpec) Validate() error {
	if !modelNamePattern.MatchString(m.Name) {
		return fmt.Errorf("invalid model name %q", m.Name)
	}
	switch modelKind(m.Kind) {
	case kindImage, kindVideo:
	default:
		return fmt.Errorf(
			"model %q declares unsupported kind %q; this driver serves the Wan image and video families (kind \"image\" or \"video\") — text generation and embeddings ride the openai driver's compatibility endpoint (impl: openai)",
			m.Name,
			m.Kind,
		)
	}
	if m.WireModel != "" && !modelNamePattern.MatchString(m.WireModel) {
		return fmt.Errorf("model %q has invalid wire_model %q", m.Name, m.WireModel)
	}
	if !m.Image.empty() && modelKind(m.Kind) != kindImage {
		return fmt.Errorf("model %q sets image parameters on kind %q", m.Name, m.Kind)
	}
	if !m.Video.empty() && modelKind(m.Kind) != kindVideo {
		return fmt.Errorf("model %q sets video parameters on kind %q", m.Name, m.Kind)
	}
	if err := m.Image.Validate(); err != nil {
		return fmt.Errorf("model %q: image: %w", m.Name, err)
	}
	if err := m.Video.Validate(); err != nil {
		return fmt.Errorf("model %q: video: %w", m.Name, err)
	}
	if err := m.Capabilities.Validate(); err != nil {
		return fmt.Errorf("model %q: %w", m.Name, err)
	}
	return m.Limits.Validate()
}

// Validate checks the image declaration for structural sanity.
func (p ImageParams) Validate() error {
	switch p.API {
	case imageAPIAsync, imageAPISync:
	default:
		return fmt.Errorf(
			"unknown api %q, want %q (async task pipeline) or %q",
			p.API, imageAPIAsync, imageAPISync,
		)
	}
	if p.PromptLimit < 0 {
		return fmt.Errorf("prompt_limit must not be negative")
	}
	if p.CountMax < 0 {
		return fmt.Errorf("count_max must not be negative")
	}
	if p.MinEdge < 0 || p.MaxEdge < 0 || p.MinPixels < 0 || p.MaxPixels < 0 {
		return fmt.Errorf("size bounds must not be negative")
	}
	if p.MaxEdge > 0 && p.MinEdge > p.MaxEdge {
		return fmt.Errorf("min_edge %d exceeds max_edge %d", p.MinEdge, p.MaxEdge)
	}
	if p.MaxPixels > 0 && p.MinPixels > p.MaxPixels {
		return fmt.Errorf("min_pixels %d exceeds max_pixels %d", p.MinPixels, p.MaxPixels)
	}
	for _, size := range p.Sizes {
		if _, err := ParseSize(size); err != nil {
			return fmt.Errorf("size %q: %w", size, err)
		}
	}
	if p.Edit && p.ReferenceImage && p.API != imageAPISync {
		// The edit surface reads its own image fields (base_image_url,
		// mask_image_url) and the text2image surface reads ref_img; a model
		// claiming both would be ambiguous about where an input image lands,
		// so the pair rejects rather than silently preferring one.
		return fmt.Errorf(
			"reference_image and edit cannot both be declared: an input image has no single home",
		)
	}
	if p.Format != "" {
		if err := p.Format.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// empty reports whether the declaration names no image-surface fact. The
// struct holds slices, so it cannot be compared to its zero value directly.
func (p ImageParams) empty() bool {
	return p.API == "" && !p.Edit && !p.ReferenceImage && !p.Style &&
		p.PromptLimit == 0 && p.CountMax == 0 &&
		p.MinEdge == 0 && p.MaxEdge == 0 &&
		p.MinPixels == 0 && p.MaxPixels == 0 &&
		p.Format == "" && len(p.Sizes) == 0
}

// empty reports whether the declaration names no video-surface fact.
func (p VideoParams) empty() bool {
	return p.Input == "" && p.SizeParam == "" &&
		p.MinDuration == 0 && p.MaxDuration == 0 &&
		!p.Audio && !p.AudioSwitch && !p.ShotType && !p.Template &&
		p.Media == nil && !p.NegativePrompt && !p.SmartDuration &&
		p.PromptLimit == 0 &&
		len(p.Resolutions) == 0 && len(p.Ratios) == 0 &&
		len(p.Sizes) == 0 && len(p.Durations) == 0
}

// Validate checks the media declaration for structural sanity.
func (p MediaParams) Validate() error {
	if p.ReferenceImages < 0 || p.ReferenceVideos < 0 || p.ReferenceAudios < 0 {
		return fmt.Errorf("reference channel caps must not be negative")
	}
	if !p.frames() && !p.references() && !p.File && !p.Link {
		return fmt.Errorf(
			"media declares no channel; name first_frame or last_frame, a reference cap (reference_images, reference_videos, reference_audios), file, or link",
		)
	}
	if p.frames() && (p.references() || p.File || p.Link) {
		return fmt.Errorf(
			"first_frame/last_frame cannot be declared with reference or document channels: the platform refuses the two modes in one request, and a lone input image would have no single home",
		)
	}
	if p.LastFrame && !p.FirstFrame {
		return fmt.Errorf("last_frame needs first_frame: a last frame has no first to end")
	}
	return nil
}

// frames reports whether the declaration accepts first/last frames.
func (p MediaParams) frames() bool { return p.FirstFrame || p.LastFrame }

// references reports whether the declaration accepts reference assets.
func (p MediaParams) references() bool {
	return p.ReferenceImages > 0 || p.ReferenceVideos > 0 || p.ReferenceAudios > 0
}

// Validate checks the video declaration for structural sanity.
func (p VideoParams) Validate() error {
	switch p.Input {
	case "", videoInputText, videoInputImage, videoInputKeyframes:
	default:
		return fmt.Errorf(
			"unknown input %q, want %q, %q, or %q",
			p.Input, videoInputText, videoInputImage, videoInputKeyframes,
		)
	}
	switch p.SizeParam {
	case "", videoSizeResolution, videoSizeConcrete:
	default:
		return fmt.Errorf(
			"unknown size_param %q, want %q or %q",
			p.SizeParam, videoSizeResolution, videoSizeConcrete,
		)
	}
	for _, tier := range p.Resolutions {
		if !resolutionPattern.MatchString(tier) {
			return fmt.Errorf("resolution %q must be a tier token like 720P", tier)
		}
	}
	for _, ratio := range p.Ratios {
		if err := media.AspectRatio(ratio).Validate(); err != nil {
			return fmt.Errorf("ratio %q: %w", ratio, err)
		}
	}
	for tier, ratios := range p.Sizes {
		if !resolutionPattern.MatchString(tier) {
			return fmt.Errorf("size tier %q must be a tier token like 720P", tier)
		}
		for ratio, size := range ratios {
			if err := media.AspectRatio(ratio).Validate(); err != nil {
				return fmt.Errorf("size %s %s: %w", tier, ratio, err)
			}
			if _, err := ParseSize(size); err != nil {
				return fmt.Errorf("size %s %s %q: %w", tier, ratio, size, err)
			}
		}
	}
	for _, duration := range p.Durations {
		if duration <= 0 {
			return fmt.Errorf("duration %d must be positive", duration)
		}
	}
	if p.MinDuration < 0 || p.MaxDuration < 0 {
		return fmt.Errorf("duration bounds must not be negative")
	}
	if p.MaxDuration > 0 && p.MinDuration > p.MaxDuration {
		return fmt.Errorf("min_duration %d exceeds max_duration %d", p.MinDuration, p.MaxDuration)
	}
	if p.PromptLimit < 0 {
		return fmt.Errorf("prompt_limit must not be negative")
	}
	if p.Media != nil {
		if err := p.Media.Validate(); err != nil {
			return err
		}
		if p.Input != "" {
			return fmt.Errorf(
				"input %q and media cannot both be declared: the media array carries its own input shape",
				p.Input,
			)
		}
		if p.SizeParam != "" {
			return fmt.Errorf(
				"size_param and media cannot both be declared: the media surface takes the resolution tier",
			)
		}
		if p.Audio {
			return fmt.Errorf(
				"audio and media cannot both be declared: the media array carries input audio as reference_audio entries",
			)
		}
	}
	if p.SmartDuration && p.Media == nil {
		return fmt.Errorf(
			"smart_duration needs media: only the media array documents the duration -1 request",
		)
	}
	return nil
}

// Validate checks the provider spec for structural sanity.
func (s Spec) Validate() error {
	if s.BaseURL != "" {
		if err := validateURL("base_url", s.BaseURL); err != nil {
			return err
		}
	}
	if s.WorkspaceID != "" && !workspaceIDPattern.MatchString(s.WorkspaceID) {
		return fmt.Errorf("invalid workspace_id %q", s.WorkspaceID)
	}
	if s.HTTPRetries != nil && *s.HTTPRetries < 0 {
		return fmt.Errorf("http_retries must not be negative")
	}
	if s.PollIntervalMillis < 0 {
		return fmt.Errorf("poll_interval_millis must not be negative")
	}
	seen := make(map[string]struct{}, len(s.Models))
	for _, declared := range s.Models {
		if err := declared.Validate(); err != nil {
			return err
		}
		if _, exists := seen[declared.Name]; exists {
			return fmt.Errorf("duplicate model declaration %q", declared.Name)
		}
		seen[declared.Name] = struct{}{}
	}
	return nil
}

// baseURL resolves the API host, defaulting to the documented public host.
func (s Spec) baseURL() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return DefaultBaseURL
}

// pollInterval paces task polling; the default is 5 seconds.
func (s Spec) pollInterval() time.Duration {
	if s.PollIntervalMillis < 1 {
		return defaultPollInterval
	}
	return time.Duration(s.PollIntervalMillis) * time.Millisecond
}

// validateURL rejects anything that is not an http(s) URL.
func validateURL(name, value string) error {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return fmt.Errorf("%s must be an http(s) URL", name)
	}
	return nil
}

func decodeSpec(ctx context.Context, raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](ctx, raw)
	if err != nil {
		return Spec{}, fmt.Errorf("dashscope spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("dashscope spec: %w", err)
	}
	return spec, nil
}

func decodeProfileSpec(ctx context.Context, raw []byte) (ProfileSpec, error) {
	spec, err := resource.DecodeTyped[ProfileSpec](ctx, raw)
	if err != nil {
		return ProfileSpec{}, fmt.Errorf("dashscope profile spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, fmt.Errorf("dashscope profile spec: %w", err)
	}
	return spec, nil
}

// ParseSize reads the platform's 宽*高 size token. It accepts the "*" the
// docs use and the "x"/"×" spellings users paste from elsewhere, because
// the value is a declaration the deployment owns, not a wire literal.
func ParseSize(value string) (media.ImageSize, error) {
	width, height, ok := cutSize(value)
	if !ok {
		return media.ImageSize{}, fmt.Errorf("size must use the form 宽*高")
	}
	w, wErr := strconv.Atoi(strings.TrimSpace(width))
	h, hErr := strconv.Atoi(strings.TrimSpace(height))
	if wErr != nil || hErr != nil || w <= 0 || h <= 0 {
		return media.ImageSize{}, fmt.Errorf("size %q must carry positive integers", value)
	}
	return media.ImageSize{Width: w, Height: h}, nil
}

func cutSize(value string) (string, string, bool) {
	for _, separator := range []string{"*", "x", "X", "×"} {
		if width, height, ok := strings.Cut(value, separator); ok {
			return width, height, true
		}
	}
	return "", "", false
}

// SizeToken renders a size the way the wire wants it: 宽*高, always with the
// "*" separator.
func SizeToken(size media.ImageSize) string {
	return strconv.Itoa(size.Width) + "*" + strconv.Itoa(size.Height)
}

// NormalizeResolution folds a canonical tier token ("720p") onto the
// platform's uppercase spelling ("720P"). The canonical vocabulary is
// lowercase by convention; the wire is not.
func NormalizeResolution(tier string) string {
	return strings.ToUpper(strings.TrimSpace(tier))
}

// sizeFor resolves the concrete size a size-param model wants for a tier and
// ratio pair.
func (p VideoParams) sizeFor(tier, ratio string) (string, bool) {
	ratios, ok := p.Sizes[tier]
	if !ok {
		return "", false
	}
	size, ok := ratios[ratio]
	return size, ok
}

// declaresResolution reports whether the given tier is acceptable: inside a
// declared list, or unconstrained when the declaration names none.
func (p VideoParams) declaresResolution(tier string) bool {
	return len(p.Resolutions) == 0 || slices.Contains(p.Resolutions, tier)
}

// declaresRatio reports whether the given ratio is acceptable: inside a
// declared list, otherwise inside the ratios the size table is keyed by,
// otherwise unconstrained because the declaration names neither. A size-table
// model constrains its ratios through the table even when it lists none
// separately, so the two are read in that order.
func (p VideoParams) declaresRatio(ratio string) bool {
	if len(p.Ratios) > 0 {
		return slices.Contains(p.Ratios, ratio)
	}
	if len(p.Sizes) > 0 {
		for _, ratios := range p.Sizes {
			if _, ok := ratios[ratio]; ok {
				return true
			}
		}
		return false
	}
	return true
}

// ratioHint names the ratios a rejection message can offer: the declared list
// when there is one, otherwise the union the size table is keyed by, in a
// stable order.
func (p VideoParams) ratioHint() string {
	if len(p.Ratios) > 0 {
		return lists(p.Ratios)
	}
	seen := make(map[string]struct{})
	ratios := make([]string, 0, len(p.Sizes))
	for _, table := range p.Sizes {
		for ratio := range table {
			if _, ok := seen[ratio]; ok {
				continue
			}
			seen[ratio] = struct{}{}
			ratios = append(ratios, ratio)
		}
	}
	slices.Sort(ratios)
	return lists(ratios)
}

// tierHint names the tiers a size-param model can resolve: its declared
// resolutions when it has them, otherwise the tiers its size table covers.
func (p VideoParams) tierHint() string {
	if len(p.Resolutions) > 0 {
		return lists(p.Resolutions)
	}
	tiers := make([]string, 0, len(p.Sizes))
	for tier := range p.Sizes {
		tiers = append(tiers, tier)
	}
	slices.Sort(tiers)
	return lists(tiers)
}

// declaresDuration reports whether the given whole-second duration is
// acceptable: inside the declared enum, or inside the declared range.
func (p VideoParams) declaresDuration(seconds int) bool {
	if len(p.Durations) > 0 && !slices.Contains(p.Durations, seconds) {
		return false
	}
	if p.MinDuration > 0 && seconds < p.MinDuration {
		return false
	}
	if p.MaxDuration > 0 && seconds > p.MaxDuration {
		return false
	}
	return true
}

// lists renders a string list for rejection messages.
func lists(values []string) string {
	return strings.Join(values, ", ")
}

// sizePairs renders the declared tier/ratio sizes for rejection messages, in
// a stable tier-then-ratio order so a message does not depend on map
// iteration.
func (p VideoParams) sizePairs() string {
	tiers := make([]string, 0, len(p.Sizes))
	for tier := range p.Sizes {
		tiers = append(tiers, tier)
	}
	slices.Sort(tiers)
	pairs := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		ratios := make([]string, 0, len(p.Sizes[tier]))
		for ratio := range p.Sizes[tier] {
			ratios = append(ratios, ratio)
		}
		slices.Sort(ratios)
		for _, ratio := range ratios {
			pairs = append(pairs, tier+" "+ratio+" = "+p.Sizes[tier][ratio])
		}
	}
	return lists(pairs)
}

// durationList renders the accepted durations for rejection messages: the
// declared enum when there is one, otherwise the declared range.
func (p VideoParams) durationList() string {
	if len(p.Durations) > 0 {
		values := make([]string, 0, len(p.Durations))
		for _, seconds := range p.Durations {
			values = append(values, fmt.Sprintf("%d s", seconds))
		}
		return lists(values)
	}
	switch {
	case p.MinDuration > 0 && p.MaxDuration > 0:
		return fmt.Sprintf(
			"at least %d s and at most %d s", p.MinDuration, p.MaxDuration,
		)
	case p.MinDuration > 0:
		return fmt.Sprintf("at least %d s", p.MinDuration)
	case p.MaxDuration > 0:
		return fmt.Sprintf("at most %d s", p.MaxDuration)
	default:
		return "the declared durations"
	}
}

// durationReason phrases a rejected duration against the declaration's shape:
// the enum the value has to be one of, or the range it has to fall inside.
func (p VideoParams) durationReason(name string, seconds int) string {
	if len(p.Durations) > 0 {
		return fmt.Sprintf(
			"%s accepts the durations %s, not %d s",
			name, p.durationList(), seconds,
		)
	}
	return fmt.Sprintf(
		"%s takes %s, not %d s", name, p.durationList(), seconds,
	)
}
