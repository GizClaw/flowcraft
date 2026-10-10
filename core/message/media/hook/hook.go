// Package hook adapts media attachment handling to the agent
// lifecycle: it registers the media.attachments prepare hook, which
// makes the seeded user message drivable by any provider before the
// engine reads the board.
//
// The hook normalizes the first (user) message on the main channel:
//
//   - audio, video and file parts the deployment does not pass through
//     are flattened into text lines naming their path, so a turn whose
//     routed model declares no such input still runs and the file tools
//     can still reach the attachment by path;
//   - URL-sourced image, audio and video parts that point at a local
//     file are rewritten as inline byte parts, because no provider can
//     fetch a path off the host;
//   - images and text otherwise pass through untouched: a vision model
//     takes the image part directly, and the declaration checks route a
//     turn to a target that accepts it.
//
// The request message itself is never rewritten; flattening and
// inlining live on the board the engine reads, so anything archiving
// the request keeps the original attachments.
package hook

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/message/media/imageutil"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils/filetype"
	"github.com/GizClaw/flowcraft/core/utils/pathsafe"
)

const (
	// AttachmentsType is the factory impl name of the media.attachments
	// prepare hook (kind hook.prepare).
	AttachmentsType = "media.attachments"

	// DefaultAudioMarker, DefaultVideoMarker and DefaultFileMarker
	// prefix the flattened text line of the matching attachment kind. A
	// deployment can replace each through the matching settings field.
	DefaultAudioMarker = "[audio file] "
	DefaultVideoMarker = "[video file] "
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
	// are never flattened.
	PassthroughKinds []string `json:"passthrough_kinds,omitempty"`
	// AudioMarker, VideoMarker and FileMarker prefix the flattened text
	// line of that kind. An empty marker falls back to the matching
	// Default*Marker.
	AudioMarker string `json:"audio_marker,omitempty"`
	VideoMarker string `json:"video_marker,omitempty"`
	FileMarker  string `json:"file_marker,omitempty"`
}

// resolvedSettings is the validated settings form the preparer closure
// uses.
type resolvedSettings struct {
	workDir     string
	passthrough map[message.PartKind]bool
	audioMarker string
	videoMarker string
	fileMarker  string
}

// resolve validates the settings and applies marker defaults.
func (s Settings) resolve() (resolvedSettings, error) {
	resolved := resolvedSettings{
		workDir:     s.WorkDir,
		passthrough: make(map[message.PartKind]bool, len(s.PassthroughKinds)),
		audioMarker: markerOr(s.AudioMarker, DefaultAudioMarker),
		videoMarker: markerOr(s.VideoMarker, DefaultVideoMarker),
		fileMarker:  markerOr(s.FileMarker, DefaultFileMarker),
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
		_ context.Context, _ agent.Identity, req *agent.Request, prev *agent.Board,
	) (*agent.Board, error) {
		if req == nil || prev == nil {
			return nil, errdefs.Validationf(
				"%s: request and previous board are required", AttachmentsType)
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
		parts, flattened := flattenNonImageMedia(channel[0].Content.Parts, config)
		parts, inlined, err := inlineLocalMedia(parts)
		if err != nil {
			return nil, err
		}
		if !flattened && !inlined {
			return board, nil
		}
		channel[0].Content.Parts = parts
		board.SetChannel(agent.MainChannel, channel)
		return board, nil
	}), nil
}

// flattenNonImageMedia replaces every audio, video or file part the
// deployment does not pass through with a text line naming the
// attachment path, so any driver can accept the turn. It reports
// whether anything changed.
func flattenNonImageMedia(
	parts []message.Part, settings resolvedSettings,
) ([]message.Part, bool) {
	out := make([]message.Part, 0, len(parts))
	changed := false
	for _, part := range parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			out = append(out, part)
			continue
		}
		switch p := normalized.(type) {
		case message.AudioPart:
			if settings.passthrough[message.PartAudio] {
				out = append(out, p)
				continue
			}
			changed = true
			out = append(out, message.TextPart{
				Text: settings.audioMarker + settings.workPath(p.Source.URL()),
			})
		case message.VideoPart:
			if settings.passthrough[message.PartVideo] {
				out = append(out, p)
				continue
			}
			changed = true
			out = append(out, message.TextPart{
				Text: settings.videoMarker + settings.workPath(p.Source.URL()),
			})
		case message.FilePart:
			if settings.passthrough[message.PartFile] {
				out = append(out, p)
				continue
			}
			changed = true
			out = append(out, message.TextPart{
				Text: settings.fileMarker + settings.workPath(p.URI),
			})
		default:
			out = append(out, p)
		}
	}
	if !changed {
		return parts, false
	}
	return out, true
}

// workPath renders an attachment path relative to the configured work
// dir when it lives under it (the model sees
// "sessions/…/media/1-a.mp3"), and falls back to the absolute path
// otherwise.
func (s resolvedSettings) workPath(path string) string {
	if s.workDir != "" {
		if rel, ok := pathsafe.Rel(s.workDir, path); ok {
			return rel
		}
	}
	return path
}

// inlineLocalMedia rewrites URL-sourced image, audio and video parts
// whose URL points at a local file into inline byte parts. Remote URLs
// (http/https/data:) and non-media parts pass through unchanged.
func inlineLocalMedia(parts []message.Part) ([]message.Part, bool, error) {
	out := make([]message.Part, 0, len(parts))
	changed := false
	for _, part := range parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return nil, false, err
		}
		switch p := normalized.(type) {
		case message.ImagePart:
			if p.Source.Kind() == media.SourceURL {
				if path, ok := localPath(p.Source.URL()); ok {
					data, err := readInline(path, "image")
					if err != nil {
						return nil, false, err
					}
					source, err := media.NewImageBytes(
						data, mediaTypeOr(path, p.Source.MediaType()))
					if err != nil {
						return nil, false, err
					}
					out = append(out, message.ImagePart{Source: source})
					changed = true
					continue
				}
			}
			out = append(out, p)
		case message.AudioPart:
			if p.Source.Kind() == media.SourceURL {
				if path, ok := localPath(p.Source.URL()); ok {
					data, err := readInline(path, "audio")
					if err != nil {
						return nil, false, err
					}
					source, err := media.NewAudioBytes(
						data, mediaTypeOr(path, p.Source.MediaType()))
					if err != nil {
						return nil, false, err
					}
					out = append(out, message.AudioPart{
						Source:         source,
						Format:         p.Format,
						DurationMillis: p.DurationMillis,
					})
					changed = true
					continue
				}
			}
			out = append(out, p)
		case message.VideoPart:
			if p.Source.Kind() == media.SourceURL {
				if path, ok := localPath(p.Source.URL()); ok {
					data, err := readInline(path, "video")
					if err != nil {
						return nil, false, err
					}
					source, err := media.NewVideoBytes(
						data, mediaTypeOr(path, p.Source.MediaType()))
					if err != nil {
						return nil, false, err
					}
					out = append(out, message.VideoPart{Source: source})
					changed = true
					continue
				}
			}
			out = append(out, p)
		default:
			out = append(out, p)
		}
	}
	return out, changed, nil
}

// readInline loads one local attachment within the shared inline
// budget.
func readInline(path, kind string) ([]byte, error) {
	if err := checkSize(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("media: read %s %s: %w", kind, path, err)
	}
	return data, nil
}

// localPath resolves a part URL that points at a local file: plain
// paths and file:// URLs that exist as regular files. Remote and data
// URLs report false and pass through.
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

// mediaTypeOr returns mediaType when the caller declared one, and
// otherwise the type classified from the file's content, so an archived
// attachment stored without a type is described by its bytes rather than
// its name.
func mediaTypeOr(path, mediaType string) string {
	if mediaType != "" {
		return mediaType
	}
	return filetype.OfPath(path).MediaType
}

// checkSize rejects an attachment over the shared inline budget before
// it is read.
func checkSize(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > imageutil.MaxInlineImageBytes {
		return fmt.Errorf(
			"media: %s exceeds the %d-byte inline limit",
			path, imageutil.MaxInlineImageBytes)
	}
	return nil
}

// markerOr applies a marker default: an unset marker keeps the
// package default.
func markerOr(marker, fallback string) string {
	if marker == "" {
		return fallback
	}
	return marker
}
