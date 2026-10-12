package dashscope

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// The Wan 3.0 all-in-one models take their assets through input.media: an
// array of typed entries instead of the flat 2.x keys, where the array order
// numbers each type for the prompt ("图1", "视频1", "音频1") and a request
// combines reference assets but never a frame with anything else. mediaInput
// accumulates that array while the compiler walks the request parts, so the
// declaration decides what a part becomes and what the request may carry.
const (
	mediaEntryFirstFrame     = "first_frame"
	mediaEntryLastFrame      = "last_frame"
	mediaEntryReferenceImage = "reference_image"
	mediaEntryReferenceVideo = "reference_video"
	mediaEntryReferenceAudio = "reference_audio"
	mediaEntryFile           = "file"
	mediaEntryLink           = "link"
)

// mediaEntry is one input.media element.
type mediaEntry struct {
	Type string
	URL  string
}

// mediaInput collects the media entries of one request in the order the
// caller wrote them.
type mediaInput struct {
	declared *MediaParams
	entries  []mediaEntry
}

func newMediaInput(declared *MediaParams) mediaInput {
	return mediaInput{declared: declared}
}

// enabled reports whether the surface rides the media array at all.
func (m mediaInput) enabled() bool { return m.declared != nil }

// count reports how many entries of one type the request carries.
func (m mediaInput) count(entryType string) int {
	count := 0
	for _, entry := range m.entries {
		if entry.Type == entryType {
			count++
		}
	}
	return count
}

// acceptImage maps an image part onto the entry the declaration names: the
// frames surface takes the first image as first_frame and the second as
// last_frame, the reference surface takes each image as reference_image while
// its cap holds. A third image on a declaration without reference channels
// has no home and rejects.
func (m *mediaInput) acceptImage(
	source media.ImageSource,
	field inference.FieldID,
	ledger *inference.Ledger,
) {
	switch source.Kind() {
	case media.SourceURL, media.SourceInline:
	case media.SourceStream:
		ledger.Reject(field, "stream media sources must be materialized before generate")
		return
	}
	// The media array documents image URLs and base64 data, so an inline
	// image rides the entry as a data URI.
	value, err := imageSourceValue(source)
	if err != nil {
		ledger.Reject(field, err.Error())
		return
	}
	switch {
	case m.declared.FirstFrame && m.count(mediaEntryFirstFrame) == 0:
		m.entries = append(m.entries, mediaEntry{Type: mediaEntryFirstFrame, URL: value})
	case m.declared.LastFrame && m.count(mediaEntryLastFrame) == 0 &&
		m.count(mediaEntryFirstFrame) == 1:
		m.entries = append(m.entries, mediaEntry{Type: mediaEntryLastFrame, URL: value})
	case m.declared.ReferenceImages > 0:
		if m.count(mediaEntryReferenceImage) >= m.declared.ReferenceImages {
			ledger.Reject(field, fmt.Sprintf(
				"the media array takes at most %d reference images",
				m.declared.ReferenceImages,
			))
			return
		}
		m.entries = append(m.entries, mediaEntry{Type: mediaEntryReferenceImage, URL: value})
	default:
		ledger.Reject(field, m.imageReason())
	}
}

// imageReason explains why the declaration has no home for another image.
func (m mediaInput) imageReason() string {
	if m.declared.frames() {
		return "the frames surface takes one first frame and, when declared, one last frame"
	}
	return "this model declares no reference images"
}

// acceptVideo maps a video part onto a reference_video entry.
func (m *mediaInput) acceptVideo(
	source media.VideoSource,
	field inference.FieldID,
	ledger *inference.Ledger,
) {
	if m.declared.ReferenceVideos == 0 {
		ledger.Reject(field, m.referenceReason("videos"))
		return
	}
	switch source.Kind() {
	case media.SourceURL:
	case media.SourceStream:
		ledger.Reject(field, "stream media sources must be materialized before generate")
		return
	default:
		ledger.Reject(field,
			"the media array takes a reference video URL; inline bytes have to be uploaded first")
		return
	}
	if m.count(mediaEntryReferenceVideo) >= m.declared.ReferenceVideos {
		ledger.Reject(field, fmt.Sprintf(
			"the media array takes at most %d reference videos",
			m.declared.ReferenceVideos,
		))
		return
	}
	m.entries = append(m.entries, mediaEntry{Type: mediaEntryReferenceVideo, URL: source.URL()})
}

// acceptAudio maps an audio part onto a reference_audio entry.
func (m *mediaInput) acceptAudio(
	source media.AudioSource,
	field inference.FieldID,
	ledger *inference.Ledger,
) {
	if m.declared.ReferenceAudios == 0 {
		ledger.Reject(field, m.referenceReason("audios"))
		return
	}
	switch source.Kind() {
	case media.SourceURL:
	case media.SourceStream:
		ledger.Reject(field, "stream media sources must be materialized before generate")
		return
	default:
		ledger.Reject(field,
			"the media array takes a reference audio URL; inline bytes have to be uploaded first")
		return
	}
	if m.count(mediaEntryReferenceAudio) >= m.declared.ReferenceAudios {
		ledger.Reject(field, fmt.Sprintf(
			"the media array takes at most %d reference audios",
			m.declared.ReferenceAudios,
		))
		return
	}
	m.entries = append(m.entries, mediaEntry{Type: mediaEntryReferenceAudio, URL: source.URL()})
}

// referenceReason phrases a rejected reference part: the frames surface
// refuses every other asset kind, so the reason names that rule rather than
// the channel the caller happened to use.
func (m mediaInput) referenceReason(plural string) string {
	if m.declared.frames() {
		return fmt.Sprintf(
			"the frames surface takes no reference %s: the platform refuses first/last frames together with other media",
			plural,
		)
	}
	return "this model declares no reference " + plural
}

// acceptFile maps a file part onto a file entry, which the platform reads as
// a document to understand rather than as media to show.
func (m *mediaInput) acceptFile(
	part message.FilePart,
	field inference.FieldID,
	ledger *inference.Ledger,
) {
	if !m.declared.File {
		ledger.Reject(field, "this model declares no file input")
		return
	}
	if m.count(mediaEntryFile) > 0 {
		ledger.Reject(field, "the media array takes one file")
		return
	}
	if !documentURL(part.URI) {
		ledger.Reject(field, "a file entry has to be an http(s) or oss:// URL")
		return
	}
	m.entries = append(m.entries, mediaEntry{Type: mediaEntryFile, URL: part.URI})
}

// documentURL reports whether a file URI is one the platform can fetch: a
// public URL, or the OSS temporary URL an upload produced. A local path is
// something the endpoint cannot read.
func documentURL(uri string) bool {
	for _, prefix := range []string{"http://", "https://", "oss://"} {
		if strings.HasPrefix(uri, prefix) {
			return true
		}
	}
	return false
}
