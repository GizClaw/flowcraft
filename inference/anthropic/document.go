package anthropic

import (
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
)

// Document is the written form of [Settings]: exactly the fields a deployment may set.
//
// It lives beside the implementation for the reason [Document] in the openai package states:
// a hand-copied settings struct in the registering binary drifts, and nothing fails when it
// does. `HTTPClient` is the one field of [Settings] that is absent, because it is a live object
// rather than a value.
type Document struct {
	Models             []model.ModelDescriptor `json:"models"`
	BaseURL            string                  `json:"base_url"`
	APIKey             string                  `json:"api_key"`
	Headers            map[string]string       `json:"headers"`
	Timeout            string                  `json:"timeout"`
	Retries            int                     `json:"retries"`
	SchemaCacheEntries int                     `json:"schema_cache_entries"`
	Wire               WireDocument            `json:"wire"`
}

// WireDocument is the written form of [Wire].
type WireDocument struct {
	VideoInput     bool   `json:"video_input"`
	ReasoningScope string `json:"reasoning_scope"`
}

// Settings resolves the written form into the value the driver takes.
func (d Document) Settings() (Settings, error) {
	timeout, err := parseDuration("timeout", d.Timeout)
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		Models:             d.Models,
		BaseURL:            d.BaseURL,
		APIKey:             d.APIKey,
		Headers:            d.Headers,
		Timeout:            timeout,
		Retries:            d.Retries,
		SchemaCacheEntries: d.SchemaCacheEntries,
		Wire: Wire{
			VideoInput:     d.Wire.VideoInput,
			ReasoningScope: d.Wire.ReasoningScope,
		},
	}, nil
}

// Build is the factory a deployment table registers: document in, provider out.
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
