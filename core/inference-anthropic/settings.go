package anthropic

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/GizClaw/flowcraft/core/inference/model"
)

// ProviderID is the identity this driver answers to.
const ProviderID = "anthropic"

// DefaultBaseURL is the public Messages API root.
const DefaultBaseURL = "https://api.anthropic.com"

// DefaultTimeout bounds one HTTP call, including the response body.
const DefaultTimeout = 2 * time.Minute

// DefaultSchemaCacheEntries bounds the decoded tool-schema cache one provider
// instance keeps. An LRU means a rotating catalog only pays for its working set.
const DefaultSchemaCacheEntries = 512

// Wire declares endpoint extensions the deployment accepts on top of the
// Messages protocol.
type Wire struct {
	// VideoInput allows video content blocks on a compatible endpoint.
	VideoInput bool
	// ReasoningScope replaces the derived model/account scope for signed
	// thinking traces. Empty keeps the conservative scope.
	ReasoningScope string
}

// Settings is the deployment configuration: everything an application decides
// once. Per-call facts live on the request instead.
type Settings struct {
	// BaseURL is the API root. Empty means DefaultBaseURL.
	BaseURL string
	// APIKey authenticates every call when one key serves the deployment.
	APIKey string
	// Headers are additional request headers, applied last.
	Headers map[string]string
	// Timeout bounds one call. Zero uses DefaultTimeout.
	Timeout time.Duration
	// Retries is how many extra attempts a retryable failure gets.
	Retries int
	// SchemaCacheEntries bounds the per-instance cache of decoded tool schemas.
	// Zero uses DefaultSchemaCacheEntries; a negative value is rejected rather than
	// silently defaulted.
	SchemaCacheEntries int
	// HTTPClient overrides the transport. Nil builds one from Timeout.
	HTTPClient *http.Client
	// Wire declares provider extensions this endpoint accepts.
	Wire Wire
	// Models is the deployment's model line-up. Empty leaves the driver
	// permissive and publishes no Catalog.
	Models []model.ModelDescriptor
}

// Validate checks the settings that cannot be defaulted.
func (s Settings) Validate() error {
	if strings.TrimSpace(s.APIKey) == "" {
		return errors.New("anthropic: api key is required")
	}
	if s.Retries < 0 {
		return errors.New("anthropic: retries must not be negative")
	}
	if s.SchemaCacheEntries < 0 {
		return errors.New("anthropic: schema_cache_entries must not be negative")
	}
	if s.Timeout < 0 {
		return errors.New("anthropic: timeout must not be negative")
	}
	if err := validateModels(s.Models); err != nil {
		return err
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
	if s.SchemaCacheEntries <= 0 {
		s.SchemaCacheEntries = DefaultSchemaCacheEntries
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

func validateModels(models []model.ModelDescriptor) error {
	seen := make(map[model.ModelID]struct{}, len(models))
	for i, descriptor := range models {
		if err := descriptor.Validate(); err != nil {
			return fmt.Errorf("anthropic: model %d: %w", i, err)
		}
		if descriptor.ID.Provider != ProviderID {
			return fmt.Errorf(
				"anthropic: model %q declares provider %q, want %q",
				descriptor.ID.Name, descriptor.ID.Provider, ProviderID,
			)
		}
		if _, ok := seen[descriptor.ID]; ok {
			return fmt.Errorf("anthropic: model %q is declared twice", descriptor.ID.Name)
		}
		seen[descriptor.ID] = struct{}{}
	}
	return nil
}
