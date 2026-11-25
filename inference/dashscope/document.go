package dashscope

import (
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Document is the written form of [Spec]: exactly the fields a deployment may
// set, and nothing else.
//
// It lives here, beside the implementation, rather than in whichever binary
// registers the provider. A hand-copied settings struct in the registering
// binary drifts silently: the field it forgets keeps its default, so nothing
// fails — the field is simply never configurable, and the copy cannot notice.
//
// The [Spec] fields that are absent are absent on purpose, and document_test.go
// names each one with its reason: HTTPClient is a live object rather than a
// value, and ReasoningScope is read by nothing in this driver.
type Document struct {
	// BaseURL is the DashScope API host, without the /api/v1 suffix the
	// driver appends. Empty keeps [DefaultBaseURL]; a regional deployment
	// points it at its workspace host instead, because keys and hosts are
	// region-bound.
	BaseURL string `json:"base_url"`
	// APIKey authenticates every call. A standalone deployment writes it
	// here; a host that resolves secrets may override the value after
	// decoding.
	APIKey string `json:"api_key"`
	// WorkspaceID rides every request as X-DashScope-WorkSpace. Empty sends
	// no header, which is what primary-account keys want.
	WorkspaceID string `json:"workspace_id"`
	// PollIntervalMillis paces task polling. Zero keeps the driver's
	// five-second default, as does any declared value below one millisecond.
	PollIntervalMillis int `json:"poll_interval_millis"`
	// Timeout bounds one wire call. Empty keeps the driver's ten-minute
	// default.
	Timeout string `json:"timeout"`
	// Models is the deployment's model line-up. [ModelSpec] carries the kind,
	// capabilities, limits, lifecycle, wire name, and the image, video,
	// generate, embed, speech and transcription declarations, so a document
	// states all of them.
	Models []ModelSpec `json:"models"`
}

// Settings resolves the written form into the value the driver takes.
func (d Document) Settings() (Spec, error) {
	timeout, err := parseDuration("timeout", d.Timeout)
	if err != nil {
		return Spec{}, err
	}
	return Spec{
		BaseURL:            d.BaseURL,
		APIKey:             d.APIKey,
		WorkspaceID:        d.WorkspaceID,
		PollIntervalMillis: d.PollIntervalMillis,
		Timeout:            timeout,
		Models:             d.Models,
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
