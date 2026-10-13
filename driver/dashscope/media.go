package dashscope

import (
	"encoding/base64"

	"github.com/GizClaw/flowcraft/core/message/media"
)

// imageSourceValue renders an input image the way every DashScope media
// field expects it (ref_img, base_image_url, mask_image_url, img_url,
// first_frame_url, last_frame_url): a public URL passes through verbatim,
// and inline bytes become a data URI carrying their media type. The API
// accepts both forms in all of those positions.
func imageSourceValue(source media.ImageSource) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes()), nil
}

// audioSourceValue renders an input audio the way the video surfaces expect
// it in input.audio_url: URL verbatim, inline bytes as a data URI.
func audioSourceValue(source media.AudioSource) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return "data:" + source.MediaType() + ";base64," +
		base64.StdEncoding.EncodeToString(source.Bytes()), nil
}
