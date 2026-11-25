package openai

import (
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
)

// Document is the written form of [Settings]: exactly the fields a deployment may set, and
// nothing else.
//
// It lives here, beside the implementation, rather than in whichever binary registers the
// provider. The alternative was tried: an application keeping its own settings struct and
// converting it, which silently lost fields (`capabilities`, `label`, `lifecycle`,
// `schema_cache_entries`) because a hand-copied type drifts and nothing fails when it does.
// A provider's document surface is a property of the provider.
//
// The two fields of [Settings] that are absent are absent on purpose: `HTTPClient` is a live
// object, not a value, and `Models` here carries the full [model.ModelDescriptor] so a document
// can state capabilities, label and lifecycle — not just the id and the limits.
type Document struct {
	Models             []model.ModelDescriptor `json:"models"`
	BaseURL            string                  `json:"base_url"`
	APIKey             string                  `json:"api_key"`
	API                string                  `json:"api"`
	Organization       string                  `json:"organization"`
	Headers            map[string]string       `json:"headers"`
	Timeout            string                  `json:"timeout"`
	Retries            int                     `json:"retries"`
	SchemaCacheEntries int                     `json:"schema_cache_entries"`
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
		API:                API(d.API),
		Organization:       d.Organization,
		Headers:            d.Headers,
		Timeout:            timeout,
		Retries:            d.Retries,
		SchemaCacheEntries: d.SchemaCacheEntries,
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

// parseDuration reads a written duration. Empty means "the driver's default", which is why it is
// not an error: a document that says nothing must not have to repeat the default.
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
