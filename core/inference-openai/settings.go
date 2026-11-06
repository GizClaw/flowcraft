package openai

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3/option"

	"github.com/GizClaw/flowcraft/core/inference/model"
)

// ProviderID is the identity this driver answers to.
const ProviderID = "openai"

// DefaultBaseURL is the public OpenAI API root.
const DefaultBaseURL = "https://api.openai.com/v1"

// DefaultTimeout bounds one HTTP call, including the response body.
const DefaultTimeout = 2 * time.Minute

// API selects which wire surface the deployment speaks.
type API string

const (
	// APIResponses is the Responses API, the current surface and the default.
	APIResponses API = "responses"
	// APIChat is the Chat Completions surface, still supported and still the
	// one most OpenAI-compatible servers implement.
	APIChat API = "chat"
)

// Validate reports whether the API selector is known.
func (a API) Validate() error {
	switch a {
	case "", APIResponses, APIChat:
		return nil
	default:
		return fmt.Errorf("openai: unknown api %q", a)
	}
}

// Kind folds the empty default onto APIResponses.
func (a API) Kind() API {
	if a == "" {
		return APIResponses
	}
	return a
}

// Settings is this driver's deployment configuration: everything an application
// decides once. Per-call facts live on the request instead.
type Settings struct {
	// BaseURL is the API root, for example https://api.openai.com/v1 or
	// https://api.deepseek.com/v1. Trailing slashes are ignored.
	BaseURL string
	// APIKey authenticates every call when one key serves the whole
	// deployment.
	APIKey string
	// Organization is sent as OpenAI-Organization when set.
	Organization string
	// Headers are additional request headers, applied last.
	Headers map[string]string
	// API selects the wire surface. Empty means APIResponses.
	//
	// Everything that varies per call — sampling, output limits, reasoning
	// depth, the answer's shape — is not here: it belongs to
	// [GenerationOptions], because one provider instance serves many nodes and
	// they do not share settings.
	API API

	// Timeout bounds one call. Zero uses DefaultTimeout.
	Timeout time.Duration
	// Retries is how many extra attempts a retryable failure gets.
	Retries int
	// HTTPClient overrides the transport. Nil builds a client from Timeout.
	HTTPClient *http.Client

	// Models is the deployment's model line-up. It is the only source of model
	// facts: the driver ships none, so a model exists because a deployment
	// declared it.
	//
	// Empty means nothing is declared, and the driver stays permissive — it then
	// publishes no [inference.Catalog] and cannot refuse a model it never heard
	// of. Declaring a line-up turns on both publication and enforcement.
	Models []model.ModelDescriptor
}

// Validate checks the settings that cannot be defaulted.
func (s Settings) Validate() error {
	if err := validateModels(s.Models); err != nil {
		return err
	}
	if strings.TrimSpace(s.APIKey) == "" {
		return errors.New("openai: api key is required")
	}
	if err := s.API.Validate(); err != nil {
		return err
	}
	if s.Retries < 0 {
		return errors.New("openai: retries must not be negative")
	}
	return nil
}

// normalized fills in the defaults that make a settings value usable.
func (s Settings) normalized() Settings {
	if strings.TrimSpace(s.BaseURL) == "" {
		s.BaseURL = DefaultBaseURL
	}
	s.BaseURL = strings.TrimRight(s.BaseURL, "/")
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	return s
}

// options builds the SDK request options for one client.
func (s Settings) options() []option.RequestOption {
	opts := []option.RequestOption{
		option.WithAPIKey(s.APIKey),
		option.WithBaseURL(s.BaseURL),
		option.WithMaxRetries(s.Retries),
	}
	if s.Organization != "" {
		opts = append(opts, option.WithOrganization(s.Organization))
	}
	if s.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(s.HTTPClient))
	} else {
		opts = append(opts, option.WithHTTPClient(&http.Client{Timeout: s.Timeout}))
	}
	for key, value := range s.Headers {
		opts = append(opts, option.WithHeader(key, value))
	}
	return opts
}
