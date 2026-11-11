package dashscope

import (
	"encoding/base64"

	"github.com/GizClaw/flowcraft/core/message/media"
)

func imageSourceValue(source media.ImageSource) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return "data:" + source.MediaType() + ";base64," + base64.StdEncoding.EncodeToString(source.Bytes()), nil
}

func audioSourceValue(source media.AudioSource) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return "data:" + source.MediaType() + ";base64," + base64.StdEncoding.EncodeToString(source.Bytes()), nil
}

func videoSourceValue(source media.VideoSource) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return "data:" + source.MediaType() + ";base64," + base64.StdEncoding.EncodeToString(source.Bytes()), nil
}
