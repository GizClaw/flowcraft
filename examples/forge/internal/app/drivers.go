package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference-anthropic"
	"github.com/GizClaw/flowcraft/core/inference-openai"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// drivers is the table this binary compiles in: a deployment names one of these
// in `inference.providers[].driver`, and a name that is not here fails the build
// with the list of what is.
//
// The runtime deliberately imports no driver, so the adapter that maps a written
// settings block onto a driver's typed settings lives here, in the host. Every
// driver's `Settings` type is a Go value without tags; a JSON block is not its
// wire format, and pretending otherwise would freeze the contract.
func drivers() []runtime.Driver {
	return []runtime.Driver{
		{Name: openai.ProviderID, Build: buildOpenAI},
		{Name: anthropic.ProviderID, Build: buildAnthropic},
	}
}

// openAISettings is the written form of openai.Settings.
type openAISettings struct {
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key"`
	API          string            `json:"api"`
	Organization string            `json:"organization"`
	Headers      map[string]string `json:"headers"`
	Timeout      string            `json:"timeout"`
	Retries      int               `json:"retries"`
}

func buildOpenAI(raw json.RawMessage) (inference.Provider, error) {
	var written openAISettings
	if err := decodeSettings(raw, &written); err != nil {
		return inference.Provider{}, err
	}
	timeout, err := parseDuration("timeout", written.Timeout)
	if err != nil {
		return inference.Provider{}, err
	}
	return openai.New(openai.Settings{
		BaseURL:      written.BaseURL,
		APIKey:       written.APIKey,
		API:          openai.API(written.API),
		Organization: written.Organization,
		Headers:      written.Headers,
		Timeout:      timeout,
		Retries:      written.Retries,
	})
}

// anthropicSettings is the written form of anthropic.Settings.
type anthropicSettings struct {
	BaseURL string            `json:"base_url"`
	APIKey  string            `json:"api_key"`
	Headers map[string]string `json:"headers"`
	Timeout string            `json:"timeout"`
	Retries int               `json:"retries"`
	Wire    struct {
		VideoInput     bool   `json:"video_input"`
		ReasoningScope string `json:"reasoning_scope"`
	} `json:"wire"`
}

func buildAnthropic(raw json.RawMessage) (inference.Provider, error) {
	var written anthropicSettings
	if err := decodeSettings(raw, &written); err != nil {
		return inference.Provider{}, err
	}
	timeout, err := parseDuration("timeout", written.Timeout)
	if err != nil {
		return inference.Provider{}, err
	}
	return anthropic.New(anthropic.Settings{
		BaseURL: written.BaseURL,
		APIKey:  written.APIKey,
		Headers: written.Headers,
		Timeout: timeout,
		Retries: written.Retries,
		Wire: anthropic.Wire{
			VideoInput:     written.Wire.VideoInput,
			ReasoningScope: written.Wire.ReasoningScope,
		},
	})
}

// decodeSettings decodes one settings block strictly: a misspelled field is a
// build error, not a silently defaulted setting. An empty block is zero settings.
func decodeSettings(raw json.RawMessage, into any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	return nil
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
