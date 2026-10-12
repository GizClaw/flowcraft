// Package hook adapts media attachments to the agent lifecycle: it
// registers the media.attachments prepare hook, which makes the seeded
// user message drivable by any provider before the engine reads the
// board.
//
// The hook normalizes the first (user) message on the main channel:
//
//   - URL-sourced image, audio and video parts that point at a local
//     file are rewritten as inline byte parts, because no provider can
//     fetch a path off the host;
//   - audio, video and file parts the deployment does not pass through
//     are flattened into text lines naming their path, so a turn whose
//     routed model declares no such input still runs and the file tools
//     can still reach the attachment by path;
//   - an attachment whose bytes cannot travel is flattened the same way:
//     a local file over the inline budget, and an inline or stream
//     source with no path to name, are described in the line instead of
//     failing the turn, so how large an attachment is never decides
//     whether a turn runs;
//   - images and text otherwise pass through untouched: a vision model
//     takes the image part directly, and the declaration checks route a
//     turn to a target that accepts it.
//
// The request message itself is never rewritten; flattening and
// inlining live on the board the engine reads, so anything archiving
// the request keeps the original attachments.
//
// # Trust boundary
//
// Part URLs on the main channel are host-attested. The hook inlines
// whatever local file they name, wherever it lives: an attachment
// legitimately sits outside the workspace root — a host keeps session
// media in its own data directory — so confinement to a root is not the
// hook's to enforce. A host that maps untrusted input into parts (a chat
// adapter turning a user-supplied path into a URL part) must sanitize
// before seeding the board.
//
// WorkDir serves readability rather than security: an attachment under
// it renders relative in the flattened text and everything else renders
// absolute, and the rendering uses forward slashes on every platform so
// the same line crosses hosts unchanged.
//
// # Budgets
//
// MaxInlineBytes bounds what the hook reads, not what the prompt
// carries: the bytes of an attachment that fits travel verbatim, so a
// stored file reaches every later turn's context at its stored size.
// The prompt-side bound that keeps that context small
// (core/media/imageutil.DefaultPromptImageBytes, far below this one) is
// applied by whoever normalizes before seeding the board: a host that
// wants it runs imageutil's entry points first, and registering
// media.attachments alone downscales nothing.
package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils/filetype"
	"github.com/GizClaw/flowcraft/core/utils/pathsafe"
)

const (
	// AttachmentsType is the factory impl name of the media.attachments
	// prepare hook (kind hook.prepare).
	AttachmentsType = "media.attachments"

	// DefaultMaxInlineBytes bounds one inlined attachment: a local file
	// larger than this is flattened to a path line instead of being read
	// into the board. It matches imageutil.MaxInlineImageBytes, the
	// budget hosts persist and preview images with, so an attachment that
	// survived persistence also survives the trip into a request.
	DefaultMaxInlineBytes = 10 << 20

	// DefaultAudioMarker, DefaultVideoMarker, DefaultImageMarker and
	// DefaultFileMarker prefix the flattened text line of the matching
	// attachment kind. A deployment can replace each through the matching
	// settings field.
	DefaultAudioMarker = "[audio file] "
	DefaultVideoMarker = "[video file] "
	DefaultImageMarker = "[image file] "
	DefaultFileMarker  = "[file] "
)

// Register adds the media.attachments prepare hook factory to r.
func Register(r *resource.Registry) error {
	return r.Register(prepareFactory{})
}

// Settings declares the media.attachments hook behavior.
type Settings struct {
	// WorkDir is the workspace root. Attachment paths under it render
	// relative to it in the flattened text ("sessions/…/media/1-a.mp3");
	// paths outside it stay absolute. Optional.
	WorkDir string `json:"work_dir,omitempty"`
	// PassthroughKinds lists the non-image part kinds that survive as
	// parts instead of being flattened: audio, video or file. A
	// deployment lists a kind only when its routed models declare it as
	// input (a video-capable model, say). The default flattens every
	// non-image attachment, because the hook runs before routing and the
	// declared inputs of the eventual target are not known here. Images
	// are flattened only when their bytes cannot travel.
	PassthroughKinds []string `json:"passthrough_kinds,omitempty"`
	// MaxInlineBytes bounds one inlined attachment. Optional; the zero
	// value takes DefaultMaxInlineBytes. A negative value is rejected.
	MaxInlineBytes int64 `json:"max_inline_bytes,omitempty"`
	// AudioMarker, VideoMarker, ImageMarker and FileMarker prefix the
	// flattened text line of that kind. An empty marker falls back to the
	// matching Default*Marker.
	AudioMarker string `json:"audio_marker,omitempty"`
	VideoMarker string `json:"video_marker,omitempty"`
	ImageMarker string `json:"image_marker,omitempty"`
	FileMarker  string `json:"file_marker,omitempty"`
}

// resolvedSettings is the validated settings form the preparer closure
// uses.
type resolvedSettings struct {
	workDir     string
	passthrough map[message.PartKind]bool
	maxInline   int64
	audioMarker string
	videoMarker string
	imageMarker string
	fileMarker  string
}

// sourceView is what this hook reads off a media source. ImageSource,
// AudioSource and VideoSource share these methods, and media exports no
// common interface for them.
type sourceView interface {
	Kind() media.SourceKind
	URL() string
	MediaType() string
}

// resolve validates the settings and applies marker defaults.
func (s Settings) resolve() (resolvedSettings, error) {
	resolved := resolvedSettings{
		workDir:     s.WorkDir,
		passthrough: make(map[message.PartKind]bool, len(s.PassthroughKinds)),
		maxInline:   DefaultMaxInlineBytes,
		audioMarker: markerOr(s.AudioMarker, DefaultAudioMarker),
		videoMarker: markerOr(s.VideoMarker, DefaultVideoMarker),
		imageMarker: markerOr(s.ImageMarker, DefaultImageMarker),
		fileMarker:  markerOr(s.FileMarker, DefaultFileMarker),
	}
	switch {
	case s.MaxInlineBytes < 0:
		return resolvedSettings{}, fmt.Errorf(
			"%s: max_inline_bytes must not be negative", AttachmentsType)
	case s.MaxInlineBytes > 0:
		resolved.maxInline = s.MaxInlineBytes
	}
	for _, raw := range s.PassthroughKinds {
		kind := message.PartKind(strings.TrimSpace(raw))
		switch kind {
		case message.PartAudio, message.PartVideo, message.PartFile:
			resolved.passthrough[kind] = true
		default:
			return resolvedSettings{}, fmt.Errorf(
				"%s: unknown passthrough kind %q (want audio, video or file)",
				AttachmentsType, raw)
		}
	}
	return resolved, nil
}

// prepareFactory builds the media.attachments prepare hook.
type prepareFactory struct{}

var _ resource.Factory = prepareFactory{}

// Spec implements resource.Factory.
func (prepareFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: resource.Kind("hook." + agent.HookSlotPreparer),
		Impl: AttachmentsType,
	}
}

// New implements resource.Factory.
func (prepareFactory) New(ctx context.Context, input resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](ctx, input.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"%s: decode settings: %w", AttachmentsType, err))
	}
	config, err := settings.resolve()
	if err != nil {
		return nil, errdefs.Validation(err)
	}
	return agent.PreparerFunc(func(
		ctx context.Context,
		identity agent.Identity,
		req *agent.Request,
		prev *agent.Board,
	) (*agent.Board, error) {
		if req == nil || prev == nil {
			return nil, errdefs.Validationf(
				"%s: request and previous board are required", AttachmentsType)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// The Preparer contract requires a fresh board on every call —
		// the engine mutates what it gets back — so even the no-op paths
		// below return a clone.
		board := prev.Clone()
		// seedBoard appends the user's request as channel[0] before the
		// preparer chain runs, so the first message is the turn input.
		// Channel() hands back a copy; this hook edits the copy and
		// writes it back, because the board contract keeps a message on
		// a channel immutable.
		channel := board.Channel(agent.MainChannel)
		if len(channel) == 0 || channel[0].Role != message.RoleUser {
			return board, nil
		}
		parts, changed, err := normalizeParts(ctx, channel[0].Content.Parts, config)
		if err != nil {
			return nil, err
		}
		if !changed {
			return board, nil
		}
		channel[0].Content.Parts = parts
		board.SetChannel(agent.MainChannel, channel)
		return board, nil
	}), nil
}

// normalizeParts returns the parts the engine should read in place of
// parts, and whether the two differ.
func normalizeParts(
	ctx context.Context, parts []message.Part, settings resolvedSettings,
) ([]message.Part, bool, error) {
	out := make([]message.Part, 0, len(parts))
	changed := false
	for _, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return nil, false, errdefs.Validationf("%s: %w", AttachmentsType, err)
		}
		next, adapted, err := settings.adaptPart(ctx, normalized)
		if err != nil {
			return nil, false, err
		}
		if !adapted {
			next = normalized
		}
		changed = changed || adapted
		out = append(out, next)
	}
	if !changed {
		return parts, false, nil
	}
	return out, true, nil
}

// adaptPart returns the part the engine should read in place of part,
// and whether the two differ. A kind the deployment does not pass
// through is flattened; a media part naming a local file is inlined
// while its bytes fit the inline budget.
func (s resolvedSettings) adaptPart(
	ctx context.Context, part message.Part,
) (message.Part, bool, error) {
	switch typed := part.(type) {
	case message.ImagePart:
		return s.inlineLocalFile(ctx, part, typed.Source, s.imageMarker)
	case message.AudioPart:
		if s.passthrough[message.PartAudio] {
			return s.inlineLocalFile(ctx, part, typed.Source, s.audioMarker)
		}
		return textLine(s.audioMarker, s.sourceLabel(typed.Source)), true, nil
	case message.VideoPart:
		if s.passthrough[message.PartVideo] {
			return s.inlineLocalFile(ctx, part, typed.Source, s.videoMarker)
		}
		return textLine(s.videoMarker, s.sourceLabel(typed.Source)), true, nil
	case message.FilePart:
		if s.passthrough[message.PartFile] {
			return part, false, nil
		}
		return textLine(s.fileMarker, s.fileLabel(typed)), true, nil
	default:
		return part, false, nil
	}
}

// inlineLocalFile rewrites a URL-sourced part that points at a local
// file into an inline byte part. Other sources, and URLs no local file
// answers to, pass through untouched. A file whose bytes do not fit the
// inline budget is flattened to a path line: the part cannot travel, and
// attachment size must not decide whether the turn runs.
func (s resolvedSettings) inlineLocalFile(
	ctx context.Context,
	part message.Part,
	source sourceView,
	marker string,
) (message.Part, bool, error) {
	if source.Kind() != media.SourceURL {
		return part, false, nil
	}
	path, ok := localPath(source.URL())
	if !ok {
		return part, false, nil
	}
	data, err := readInline(ctx, path, s.maxInline)
	if errors.Is(err, errOverInlineBudget) {
		return textLine(marker, s.workPath(path)), true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return inlinePart(part, data, mediaTypeFor(path, source.MediaType(), data))
}

// inlinePart rebuilds part around bytes carried inline. The typed fields
// a part adds besides its source (the audio format and duration) are
// preserved.
func inlinePart(
	part message.Part, data []byte, mediaType string,
) (message.Part, bool, error) {
	switch typed := part.(type) {
	case message.ImagePart:
		source, err := media.NewImageBytes(data, mediaType)
		if err != nil {
			return nil, false, err
		}
		return message.ImagePart{Source: source}, true, nil
	case message.AudioPart:
		source, err := media.NewAudioBytes(data, mediaType)
		if err != nil {
			return nil, false, err
		}
		typed.Source = source
		return typed, true, nil
	case message.VideoPart:
		source, err := media.NewVideoBytes(data, mediaType)
		if err != nil {
			return nil, false, err
		}
		return message.VideoPart{Source: source}, true, nil
	default:
		return part, false, nil
	}
}

// mediaTypeFor decides the type an inlined attachment reports: the type
// its source declared, and otherwise the type classified from the bytes
// rather than from the file name.
func mediaTypeFor(path, declared string, data []byte) string {
	if declared != "" {
		return declared
	}
	return filetype.OfData(filepath.Base(path), data).MediaType
}

// workPath renders an attachment path for a flattened line: relative to
// the configured work dir when the path lives under it
// ("sessions/…/media/1-a.mp3"), absolute otherwise. Separators are
// slashed on every platform, because the line is prompt text and prompt
// text crosses hosts.
func (s resolvedSettings) workPath(path string) string {
	if s.workDir != "" {
		if rel, ok := pathsafe.Rel(s.workDir, path); ok {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}

// sourceLabel renders a media source for a flattened line: the path it
// names, and otherwise a description of what the source carries.
func (s resolvedSettings) sourceLabel(source sourceView) string {
	if source.Kind() == media.SourceURL {
		if raw := strings.TrimSpace(source.URL()); raw != "" {
			return s.workPath(raw)
		}
	}
	return describeAttachment(string(source.Kind()), source.MediaType())
}

// fileLabel renders a file part for a flattened line: the path it names,
// and otherwise its name and type.
func (s resolvedSettings) fileLabel(part message.FilePart) string {
	if uri := strings.TrimSpace(part.URI); uri != "" {
		return s.workPath(uri)
	}
	mediaType := part.MediaType
	if mediaType == "" {
		mediaType = filetype.OfPath(part.Name).MediaType
	}
	return describeAttachment(string(message.PartFile), mediaType)
}

// describeAttachment names an attachment that carries no path, so a
// model told to do without it also knows what it lost.
func describeAttachment(kind, mediaType string) string {
	if mediaType == "" {
		mediaType = "unknown type"
	}
	return fmt.Sprintf("(%s %s)", kind, mediaType)
}

// textLine builds the flattened line for one attachment.
func textLine(marker, label string) message.TextPart {
	return message.TextPart{Text: marker + label}
}

// localPath resolves a part URL that points at a local file: plain paths
// and file:// URLs that exist as regular files. Remote and data URLs
// report false and pass through.
func localPath(raw string) (string, bool) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", false
	}
	path = strings.TrimPrefix(path, "file://")
	if strings.HasPrefix(path, "http://") ||
		strings.HasPrefix(path, "https://") ||
		strings.HasPrefix(path, "data:") {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return path, true
}

// errOverInlineBudget reports an attachment whose bytes do not fit the
// inline budget. The caller flattens the part instead of failing.
var errOverInlineBudget = errors.New("attachment exceeds the inline budget")

// readInline loads one local attachment within limit bytes. The read is
// bounded, so a file that grows between the size check and the read
// cannot outgrow the budget either.
func readInline(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf(
			"%s: read attachment %s: %w", AttachmentsType, path, err)
	}
	defer func() {
		_ = f.Close() // read-only handle: a close error changes nothing here
	}()
	// The read runs one byte past the budget, which is how "over the
	// budget" is detected. At the int64 ceiling there is no byte left to
	// add, and no readable file is over that budget, so the increment is
	// skipped: wrapping it would leave a negative bound, report EOF at
	// once, and hand an empty payload to a media constructor.
	bound := limit
	if bound < math.MaxInt64 {
		bound++
	}
	data, err := io.ReadAll(io.LimitReader(f, bound))
	if err != nil {
		return nil, fmt.Errorf(
			"%s: read attachment %s: %w", AttachmentsType, path, err)
	}
	if int64(len(data)) > limit {
		return nil, errOverInlineBudget
	}
	return data, nil
}

// markerOr applies a marker default: an unset marker keeps the package
// default.
func markerOr(marker, fallback string) string {
	if marker == "" {
		return fallback
	}
	return marker
}
