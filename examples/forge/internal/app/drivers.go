package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/inference/anthropic"
	"github.com/GizClaw/flowcraft/inference/openai"
)

// providers registers the protocol families this binary carries: a deployment names one of these in
// `inference.providers[].impl`, and a name that is not here fails the load with the list of what is.
//
// The table lives in the application because the platform imports no provider. Every provider's
// `Settings` is a Go value without tags, so the adapter from a written block to that value is this
// function — the alternative would freeze one provider's settings shape as the document's.
//
// Nothing per-call is decided here. What one request asks for — sampling, retention, a vendor's own
// knob — is the model node's `request.extensions` in the graph document, which is where a turn's
// decision belongs; Settings is what the deployment decides once.
func providers() func(*deploy.Impls) *deploy.Impls {
	return func(impls *deploy.Impls) *deploy.Impls {
		return impls.
			Provider(deploy.Impl(openai.ProviderID, buildOpenAI)).
			Provider(deploy.Impl(anthropic.ProviderID, buildAnthropic))
	}
}

// openAISettings is the written form of openai.Settings.
type openAISettings struct {
	// Models is what this provider serves, as the process document declares it: the host reports these
	// as the models the process has, and an agent document selects from them.
	Models       []modelDeclaration `json:"models"`
	BaseURL      string             `json:"base_url"`
	APIKey       string             `json:"api_key"`
	API          string             `json:"api"`
	Organization string             `json:"organization"`
	Headers      map[string]string  `json:"headers"`
	Timeout      string             `json:"timeout"`
	Retries      int                `json:"retries"`
}

func buildOpenAI(written openAISettings) (*inference.Provider, error) {
	if err := written.valid(); err != nil {
		return nil, err
	}
	timeout, err := parseDuration("timeout", written.Timeout)
	if err != nil {
		return nil, err
	}
	provider, err := openai.New(openai.Settings{
		Models:       descriptors(written.Models),
		BaseURL:      written.BaseURL,
		APIKey:       written.APIKey,
		API:          openai.API(written.API),
		Organization: written.Organization,
		Headers:      written.Headers,
		Timeout:      timeout,
		Retries:      written.Retries,
	})
	if err != nil {
		return nil, err
	}
	return &provider, nil
}

// anthropicSettings is the written form of anthropic.Settings.
type anthropicSettings struct {
	Models  []modelDeclaration `json:"models"`
	BaseURL string             `json:"base_url"`
	APIKey  string             `json:"api_key"`
	Headers map[string]string  `json:"headers"`
	Timeout string             `json:"timeout"`
	Retries int                `json:"retries"`
	Wire    struct {
		VideoInput     bool   `json:"video_input"`
		ReasoningScope string `json:"reasoning_scope"`
	} `json:"wire"`
}

func buildAnthropic(written anthropicSettings) (*inference.Provider, error) {
	if err := written.valid(); err != nil {
		return nil, err
	}
	timeout, err := parseDuration("timeout", written.Timeout)
	if err != nil {
		return nil, err
	}
	provider, err := anthropic.New(anthropic.Settings{
		Models:  descriptors(written.Models),
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
	if err != nil {
		return nil, err
	}
	return &provider, nil
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

// valid is what the strict decode used to cover: a block the provider cannot honour fails here, at
// plan time, rather than on the first call.
func (s openAISettings) valid() error { return nil }

func (s anthropicSettings) valid() error { return nil }

// modelDeclaration is one model a provider serves, in the document's written form.
type modelDeclaration struct {
	ID     model.ModelID     `json:"id"`
	Limits model.ModelLimits `json:"limits"`
}

// descriptors turns the written line-up into the driver's catalogue type. The provider publishes it,
// which is how a declared model becomes one an agent document can select and the process can report.
func descriptors(declared []modelDeclaration) []model.ModelDescriptor {
	out := make([]model.ModelDescriptor, 0, len(declared))
	for _, one := range declared {
		out = append(out, model.ModelDescriptor{ID: one.ID, Limits: one.Limits})
	}
	return out
}
