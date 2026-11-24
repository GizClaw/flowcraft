package dashscope

import (
	"github.com/GizClaw/flowcraft/core/message/media"
)

func imageSourceValue(source media.ImageSource) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return media.DataURL(source.MediaType(), source.Bytes()), nil
}

func audioSourceValue(source media.AudioSource) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return media.DataURL(source.MediaType(), source.Bytes()), nil
}

func videoSourceValue(source media.VideoSource) (string, error) {
	if source.Kind() == media.SourceURL {
		return source.URL(), nil
	}
	return media.DataURL(source.MediaType(), source.Bytes()), nil
}
