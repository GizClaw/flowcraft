package files

import (
	"bytes"
	"fmt"
	"image"

	// Registers the decoders a read_file image branch can validate: the
	// standard library ships these, and they are what DetectContentType also
	// recognises. WebP is recognised by content type but has no decoder here,
	// so it is refused by name instead of read as text.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

const (
	// maxImageBytes bounds an image read. It is larger than the text window
	// limit because a screenshot easily exceeds a couple of megabytes.
	maxImageBytes = 8 << 20
	// maxImageEdge bounds one side. Above this the image is refused with an
	// instruction to downscale: this build does not resize, and silently
	// shrinking would hide that the model saw something else.
	maxImageEdge = 4096
)

// detectedImage is what a byte slice looks like to the image branch.
type detectedImage struct {
	// mediaType is the content type sniffed from the bytes.
	mediaType string
	// decoded is false when the type is recognised but this build cannot decode
	// it (webp), so the caller can refuse by name.
	decoded bool
}

// detectImage reports whether data is an image, and whether it can be decoded.
//
// Detection is by content, never by extension: a .png may hold a JPEG, and a
// JPEG may be named .txt. http.DetectContentType looks at the leading bytes,
// which is the only thing that is actually true about the file.
func detectImage(data []byte) (detectedImage, bool) {
	if len(data) < 12 {
		return detectedImage{}, false
	}
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg", "image/gif":
		return detectedImage{mediaType: http.DetectContentType(data), decoded: true}, true
	case "image/webp":
		return detectedImage{mediaType: "image/webp", decoded: false}, true
	default:
		return detectedImage{}, false
	}
}

// imageContent validates one image and returns it as tool content: the image
// part plus a text note the model can read.
func imageContent(toolName, path string, data []byte, detected detectedImage) (message.Content, error) {
	if !detected.decoded {
		return message.Content{}, validationf(
			"%s: %s is %s, which this build cannot decode; convert it to PNG or JPEG first",
			toolName, path, detected.mediaType)
	}
	if len(data) > maxImageBytes {
		return message.Content{}, validationf(
			"%s: %s is %d bytes and the image limit is %d; downscale it and read again",
			toolName, path, len(data), maxImageBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return message.Content{}, validationf(
			"%s: %s is not a decodable image: %v", toolName, path, err)
	}
	if config.Width > maxImageEdge || config.Height > maxImageEdge {
		return message.Content{}, validationf(
			"%s: %s is %dx%d and the limit is %d px per side; downscale it (for example "+
				"with sips, convert or ffmpeg in a command) and read it again",
			toolName, path, config.Width, config.Height, maxImageEdge)
	}

	source, err := media.NewImageBytes(data, detected.mediaType)
	if err != nil {
		return message.Content{}, validationf("%s: %s: %v", toolName, path, err)
	}
	note := fmt.Sprintf("image %s %dx%d, %d bytes", format, config.Width, config.Height, len(data))
	return message.Content{Parts: []message.Part{
		message.ImagePart{Source: source},
		message.TextPart{Text: note},
	}}, nil
}
