package bytedance

import "github.com/GizClaw/flowcraft/core/message/media"

// A media part arrives either as a URL or as inline bytes. Ark takes a URI
// either way, so the inline form travels as a data URL — one shape for the
// request, and no second field to keep in sync.

func sourceURI(source media.ImageSource) string {
	if source.Kind() == media.SourceURL {
		return source.URL()
	}
	return media.DataURL(source.MediaType(), source.Bytes())
}
