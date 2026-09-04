package openai

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Secret names owned by this provider. Profile secrets outside this set are
// rejected at build time so typos fail fast instead of silently missing.
const (
	// SecretAPIKey authenticates every OpenAI API surface.
	SecretAPIKey = "api_key"
)

// Spec is the provider-level configuration for OpenAI. It must stay
// credential-free: strict decoding rejects unknown keys, and credentials
// live only in profile secrets.
type Spec struct {
	// API selects the generate surface: "responses" (default) or
	// "chat" (Chat Completions). Chat mode is provider-wide and only
	// affects generate; embed / image / tts use their own endpoints.
	API string `json:"api,omitempty"`
	// ChatStreamOptions controls chat-completions stream transport when
	// API is "chat". Responses streams always carry usage in their
	// terminal event and never consult this setting.
	ChatStreamOptions *ChatStreamOptionsSpec `json:"chat_stream_options,omitempty"`
	// BaseURL overrides the API base URL (gateways, proxies, Azure-style
	// compatible endpoints).
	BaseURL string `json:"base_url,omitempty"`
	// Organization sets the OpenAI-Organization header.
	Organization string `json:"organization,omitempty"`
	// Project sets the OpenAI-Project header.
	Project string `json:"project,omitempty"`
	// HTTPRetries bounds wire-level retries inside one logical inference
	// attempt, including the first. Zero disables SDK-internal retries so
	// the route Router owns the full retry budget; nil keeps the openai-go
	// default (two retries).
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// RequestMetadata controls how canonical GenerateRequest metadata is
	// projected onto the provider request body. The empty value disables
	// forwarding; any non-empty value names the top-level body field that
	// receives the bag. "metadata" uses the native OpenAI metadata object
	// and "client_metadata" uses the Codex-style passthrough, but other
	// names are allowed for gateways.
	RequestMetadata *RequestMetadataSpec `json:"request_metadata,omitempty"`
	// Models declares additional models beyond the built-in catalog or
	// overrides catalog entries by name.
	Models []ModelSpec `json:"models,omitempty"`
}

// ChatStreamOptionsSpec is the provider-level lowering policy for Chat
// Completions streaming. It mirrors the wire-level stream_options object.
type ChatStreamOptionsSpec struct {
	// IncludeUsage sends stream_options.include_usage on chat stream
	// requests. Nil keeps the driver default (true): without the usage
	// chunk, Chat Completions streaming reports no token usage and the
	// unified result carries zero provider usage. Set false for compatible
	// endpoints that reject or ignore stream_options.
	IncludeUsage *bool `json:"include_usage,omitempty"`
	// IncludeObfuscation sends stream_options.include_obfuscation on chat
	// stream requests. Nil keeps the OpenAI default (true): deltas carry
	// stream-obfuscation fields. Set false to disable stream obfuscation
	// when the extra payload is unwanted; this keeps stream_options on the
	// wire even when IncludeUsage is false.
	IncludeObfuscation *bool `json:"include_obfuscation,omitempty"`
}

// RequestMetadataSpec is the provider-level lowering policy for canonical
// GenerateRequest.RequestMetadata. Keys and the envelope are opaque and
// forwarded verbatim.
type RequestMetadataSpec struct {
	Envelope string `json:"envelope,omitempty"`
}

// Validate checks the request metadata forwarding policy.
func (s RequestMetadataSpec) Validate() error {
	return nil
}

// ModelSpec declares one model outside the built-in catalog. Capabilities
// mirror the built-in catalog shape: content kinds, hosted web search, and
// the reasoning control capability (validated against the kind's compiler
// contract at merge time). Dimensions and EffortNone are control
// capabilities that no capability kind expresses and stay separate flags.
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Capabilities declares the model's input/output content kinds, hosted
	// web search support, and reasoning control capability.
	Capabilities inference.ModelCapabilities `json:"capabilities,omitempty"`
	// Dimensions (embed) allows custom output dimensions.
	Dimensions bool `json:"dimensions,omitempty"`
	// EffortNone marks generate models whose reasoning.effort accepts
	// "none" to disable reasoning (gpt-5.1+); models without it reject a
	// ReasoningEnabled=false request.
	EffortNone bool `json:"effort_none,omitempty"`
}

// ProfileSpec is the per-credential-profile configuration. OpenAI addresses
// models by public slug and every surface shares one API key, so no
// profile-scoped settings exist today; the struct is reserved so future
// account-scoped settings have a home without a config schema break.
type ProfileSpec struct{}

func (s ProfileSpec) Validate() error {
	return nil
}

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (s Spec) Validate() error {
	switch s.apiMode() {
	case apiResponses, apiChat:
	default:
		return fmt.Errorf("api must be \"responses\" or \"chat\"")
	}
	if s.ChatStreamOptions != nil && s.apiMode() != apiChat {
		return fmt.Errorf("chat_stream_options requires api \"chat\"")
	}
	if s.BaseURL != "" &&
		!strings.HasPrefix(s.BaseURL, "https://") &&
		!strings.HasPrefix(s.BaseURL, "http://") {
		return fmt.Errorf("base_url must be an http(s) URL")
	}
	if s.HTTPRetries != nil && *s.HTTPRetries < 0 {
		return fmt.Errorf("http_retries must not be negative")
	}
	if s.RequestMetadata != nil {
		if err := s.RequestMetadata.Validate(); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(s.Models))
	for index, model := range s.Models {
		if err := model.Validate(); err != nil {
			return fmt.Errorf("models[%d]: %w", index, err)
		}
		if _, duplicate := seen[model.Name]; duplicate {
			return fmt.Errorf("models[%d]: duplicate model %q", index, model.Name)
		}
		seen[model.Name] = struct{}{}
	}
	return nil
}

// apiMode returns the normalized generate API mode.
func (s Spec) apiMode() apiMode {
	if s.API == "" {
		return apiResponses
	}
	return apiMode(s.API)
}

func (s Spec) requestMetadataEnvelope() string {
	if s.RequestMetadata == nil {
		return ""
	}
	return s.RequestMetadata.Envelope
}

// chatStreamIncludeUsage returns the provider policy for chat streaming,
// or nil when the driver default (include usage) should apply.
func (s Spec) chatStreamIncludeUsage() *bool {
	if s.ChatStreamOptions == nil {
		return nil
	}
	return s.ChatStreamOptions.IncludeUsage
}

// chatStreamIncludeObfuscation returns the provider policy for chat stream
// obfuscation, or nil when the OpenAI default should apply.
func (s Spec) chatStreamIncludeObfuscation() *bool {
	if s.ChatStreamOptions == nil {
		return nil
	}
	return s.ChatStreamOptions.IncludeObfuscation
}

func (m ModelSpec) Validate() error {
	if !modelNamePattern.MatchString(m.Name) {
		return fmt.Errorf("invalid model name %q", m.Name)
	}
	switch modelKind(m.Kind) {
	case kindGenerate, kindEmbed, kindImage, kindTTS:
	default:
		return fmt.Errorf("model %q has unknown kind %q", m.Name, m.Kind)
	}
	return m.Capabilities.Validate()
}

func decodeSpec(ctx context.Context, raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](ctx, raw)
	if err != nil {
		return Spec{}, fmt.Errorf("openai spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("openai spec: %w", err)
	}
	return spec, nil
}

func decodeProfileSpec(ctx context.Context, raw []byte) (ProfileSpec, error) {
	spec, err := resource.DecodeTyped[ProfileSpec](ctx, raw)
	if err != nil {
		return ProfileSpec{}, fmt.Errorf("openai profile spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, fmt.Errorf("openai profile spec: %w", err)
	}
	return spec, nil
}
