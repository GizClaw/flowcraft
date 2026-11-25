package minimax

import (
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Document is the written form of [Settings]: exactly the fields a deployment
// may set, and nothing else.
//
// It lives here, beside the implementation, rather than in whichever binary
// registers the provider. A hand-copied settings struct in the registering
// binary drifts silently: the field it forgets keeps its default, so nothing
// fails — the field is simply never configurable, and the copy cannot notice.
//
// HTTPClient is the one [Settings] field that is absent: it is a live object
// rather than a value. There is no retries field on either side, because this
// driver makes one attempt per call and the route Router owns the attempt
// budget.
type Document struct {
	// BaseURL is the media API root (t2a and video). Empty keeps
	// [DefaultBaseURL], which is a host root: the driver appends the version
	// segment itself.
	BaseURL string `json:"base_url"`
	// APIKey authenticates every call. A standalone deployment writes it
	// here; a host that resolves secrets may override the value after
	// decoding.
	APIKey string `json:"api_key"`
	// Headers are request headers the deployment needs (a gateway's routing
	// header, for instance).
	Headers map[string]string `json:"headers"`
	// Timeout bounds one wire call. Empty keeps the driver's ten-minute
	// default.
	Timeout string `json:"timeout"`
	// VideoPollInterval paces video task polling. Empty keeps
	// [DefaultVideoPollInterval].
	VideoPollInterval string `json:"video_poll_interval"`
	// Models is the deployment's model line-up. [ModelSpec] carries the kind,
	// capabilities, limits, lifecycle, wire name, and the speech and video
	// surface declarations, so a document states all of them.
	Models []ModelSpec `json:"models"`
}

// Settings resolves the written form into the value the driver takes.
func (d Document) Settings() (Settings, error) {
	timeout, err := parseDuration("timeout", d.Timeout)
	if err != nil {
		return Settings{}, err
	}
	videoPollInterval, err := parseDuration("video_poll_interval", d.VideoPollInterval)
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		BaseURL:           d.BaseURL,
		APIKey:            d.APIKey,
		Headers:           d.Headers,
		Timeout:           timeout,
		VideoPollInterval: videoPollInterval,
		Models:            d.Models,
	}, nil
}

// Build is the factory a deployment table registers: document in, provider
// out.
func Build(written Document) (*inference.Provider, error) {
	settings, err := written.Settings()
	if err != nil {
		return nil, err
	}
	provider, err := New(settings)
	if err != nil {
		return nil, err
	}
	return &provider, nil
}

// parseDuration reads a written duration. Empty means "the driver's default",
// which is why it is not an error: a document that says nothing must not have
// to repeat the default.
func parseDuration(field, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", field, value, err)
	}
	return parsed, nil
}
