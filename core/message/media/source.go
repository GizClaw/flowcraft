// Package media defines operation-neutral image, audio, and video value types.
// It deliberately does not depend on the parent inference package.
package media

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/url"
	"strings"
)

type SourceKind string

const (
	SourceURL    SourceKind = "url"
	SourceInline SourceKind = "inline"
)

func (k SourceKind) Validate() error {
	switch k {
	case SourceURL, SourceInline:
		return nil
	default:
		return fmt.Errorf("unknown media source kind %q", k)
	}
}

type source struct {
	kind      SourceKind
	url       string
	data      []byte
	mediaType string
}

type (
	ImageSource struct{ source }
	AudioSource struct{ source }
	VideoSource struct{ source }
)

func (s ImageSource) Validate() error { return validateTypedSource(s.source, "image/") }
func (s AudioSource) Validate() error { return validateTypedSource(s.source, "audio/") }
func (s VideoSource) Validate() error { return validateTypedSource(s.source, "video/") }
func (s ImageSource) Clone() ImageSource {
	s.source = s.clone()
	return s
}

func (s AudioSource) Clone() AudioSource {
	s.source = s.clone()
	return s
}

func (s VideoSource) Clone() VideoSource {
	s.source = s.clone()
	return s
}

func (s source) clone() source {
	s.data = bytes.Clone(s.data)
	return s
}

func (s *ImageSource) UnmarshalJSON(data []byte) error {
	return unmarshalTypedSource(data, &s.source, "image/")
}

func (s *AudioSource) UnmarshalJSON(data []byte) error {
	return unmarshalTypedSource(data, &s.source, "audio/")
}

func (s *VideoSource) UnmarshalJSON(data []byte) error {
	return unmarshalTypedSource(data, &s.source, "video/")
}

func NewImageURL(rawURL, mediaType string) (ImageSource, error) {
	value, err := newURL(rawURL, mediaType, "image/")
	return ImageSource{source: value}, err
}

func NewAudioURL(rawURL, mediaType string) (AudioSource, error) {
	value, err := newURL(rawURL, mediaType, "audio/")
	return AudioSource{source: value}, err
}

func NewVideoURL(rawURL, mediaType string) (VideoSource, error) {
	value, err := newURL(rawURL, mediaType, "video/")
	return VideoSource{source: value}, err
}

func NewImageBytes(data []byte, mediaType string) (ImageSource, error) {
	value, err := newBytes(data, mediaType, "image/")
	return ImageSource{source: value}, err
}

func NewAudioBytes(data []byte, mediaType string) (AudioSource, error) {
	value, err := newBytes(data, mediaType, "audio/")
	return AudioSource{source: value}, err
}

func NewVideoBytes(data []byte, mediaType string) (VideoSource, error) {
	value, err := newBytes(data, mediaType, "video/")
	return VideoSource{source: value}, err
}

func newURL(rawURL, mediaType, prefix string) (source, error) {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return source{}, fmt.Errorf("media URL must be absolute")
	}
	normalized, err := normalizeMediaType(mediaType, prefix, false)
	if err != nil {
		return source{}, err
	}
	value := source{kind: SourceURL, url: rawURL, mediaType: normalized}
	return value, value.Validate()
}

func newBytes(data []byte, mediaType, prefix string) (source, error) {
	normalized, err := normalizeMediaType(mediaType, prefix, true)
	if err != nil {
		return source{}, err
	}
	value := source{
		kind:      SourceInline,
		data:      bytes.Clone(data),
		mediaType: normalized,
	}
	return value, value.Validate()
}

func validateMediaType(value, prefix string, required bool) error {
	_, err := normalizeMediaType(value, prefix, required)
	return err
}

func normalizeMediaType(value, prefix string, required bool) (string, error) {
	if value == "" {
		if required {
			return "", fmt.Errorf("inline media source media type is required")
		}
		return "", nil
	}
	parsed, parameters, err := mime.ParseMediaType(value)
	if err != nil || !strings.HasPrefix(parsed, prefix) {
		return "", fmt.Errorf("media type %q must use %s", value, prefix)
	}
	return mime.FormatMediaType(parsed, parameters), nil
}

func validateTypedSource(value source, prefix string) error {
	if err := value.Validate(); err != nil {
		return err
	}
	return validateMediaType(value.mediaType, prefix, value.kind == SourceInline)
}

func unmarshalTypedSource(data []byte, target *source, prefix string) error {
	var value source
	if err := value.UnmarshalJSON(data); err != nil {
		return err
	}
	if err := validateTypedSource(value, prefix); err != nil {
		return err
	}
	normalized, err := normalizeMediaType(
		value.mediaType,
		prefix,
		value.kind == SourceInline,
	)
	if err != nil {
		return err
	}
	value.mediaType = normalized
	*target = value
	return nil
}

func (s source) Kind() SourceKind { return s.kind }
func (s source) URL() string      { return s.url }
func (s source) Bytes() []byte    { return bytes.Clone(s.data) }
func (s source) MediaType() string {
	return s.mediaType
}

func (s source) BaseMediaType() string {
	parsed, _, err := mime.ParseMediaType(s.mediaType)
	if err != nil {
		return ""
	}
	return parsed
}

func (s source) Validate() error {
	switch s.kind {
	case SourceURL:
		if s.url == "" || len(s.data) != 0 {
			return fmt.Errorf("URL media source has invalid payload")
		}
	case SourceInline:
		if len(s.data) == 0 || s.url != "" {
			return fmt.Errorf("inline media source has invalid payload")
		}
		if s.mediaType == "" {
			return fmt.Errorf("inline media source media type is required")
		}
	default:
		return s.kind.Validate()
	}
	return nil
}

func (s source) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Kind      SourceKind `json:"kind"`
		URL       string     `json:"url,omitempty"`
		Data      []byte     `json:"data,omitempty"`
		MediaType string     `json:"media_type,omitempty"`
	}{s.kind, s.url, s.data, s.mediaType})
}

func (s *source) UnmarshalJSON(data []byte) error {
	var wire struct {
		Kind      SourceKind `json:"kind"`
		URL       string     `json:"url"`
		Data      []byte     `json:"data"`
		MediaType string     `json:"media_type"`
	}
	if err := decodeStrict(data, &wire); err != nil {
		return err
	}
	candidate := source{
		kind:      wire.Kind,
		url:       wire.URL,
		data:      bytes.Clone(wire.Data),
		mediaType: wire.MediaType,
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	*s = candidate
	return nil
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
