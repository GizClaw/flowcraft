package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Spec is the provider-level configuration for the OpenAI wire family. It is
// credential-free — strict decoding rejects unknown keys and credentials live
// only in profile secrets — and split into three orthogonal layers:
//
//   - Endpoint: where requests go and how they authenticate. Transport only,
//     safe to configure freely.
//   - Wire: what the endpoint accepts on the wire. Every leaf here narrows
//     behavior the compiler would otherwise derive, so each combination is
//     cross-checked against the catalog's capability declarations at build
//     time.
//   - Catalog: which models exist and whether the built-in OpenAI line-up is
//     inherited.
type Spec struct {
	// API selects the generate surface: "responses" (default) or
	// "chat" (Chat Completions). Chat mode is provider-wide and only
	// affects generate; embed / image / tts use their own endpoints.
	API string `json:"api,omitempty"`
	// Endpoint locates and authenticates the API.
	Endpoint EndpointSpec `json:"endpoint,omitempty"`
	// Auth names the credential transport. The value always comes from the
	// profile's api_key secret; this block only says how it rides the wire.
	Auth AuthSpec `json:"auth,omitempty"`
	// Wire declares the dialect the endpoint speaks. Empty keeps the OpenAI
	// defaults.
	Wire WireSpec `json:"wire,omitempty"`
	// Catalog selects the model namespace: "builtin_declared" (default)
	// merges spec.models over the built-in OpenAI line-up, "declared" starts
	// from an empty catalog so deployment names never inherit OpenAI facts.
	Catalog string `json:"catalog,omitempty"`
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

// EndpointSpec locates and authenticates one OpenAI-wire endpoint. Every leaf
// is transport-only: none of it changes how a request is compiled.
type EndpointSpec struct {
	// BaseURL overrides the API base URL (gateways, proxies, Azure-style
	// compatible endpoints). Empty uses DefaultBaseURL.
	BaseURL string `json:"base_url,omitempty"`
	// Routing selects deployment-path rewriting. Empty posts to the plain
	// OpenAI routes; "azure_deployment" rewrites data-plane paths to
	// /openai/deployments/{model}/... and adds the api-version query.
	Routing string `json:"routing,omitempty"`
	// Query adds query parameters to every request. An Azure deployment
	// endpoint reads api-version from here, defaulting to
	// DefaultAzureAPIVersion.
	Query map[string]string `json:"query,omitempty"`
	// Headers adds static headers to every request. Use this for gateway
	// routing hints; credentials belong in profiles.
	Headers map[string]string `json:"headers,omitempty"`
	// Organization sets the OpenAI-Organization header.
	Organization string `json:"organization,omitempty"`
	// Project sets the OpenAI-Project header.
	Project string `json:"project,omitempty"`
	// Timeout bounds one HTTP request, as a Go duration string ("90s",
	// "2m"). Empty keeps the SDK default. It bounds a single wire attempt,
	// not the whole logical inference call, which the route Router owns.
	Timeout string `json:"timeout,omitempty"`
}

// AuthSpec names the credential transport for one endpoint.
type AuthSpec struct {
	// Scheme is "bearer" (Authorization: Bearer <api_key>, the default) or
	// "header" (a named header carries the key, e.g. Azure's Api-Key).
	Scheme string `json:"scheme,omitempty"`
	// Header is the header name carrying the key when Scheme is "header".
	Header string `json:"header,omitempty"`
}

// WireSpec declares the dialect an endpoint accepts. Each leaf narrows a
// derived default, so the driver cross-checks the combination against the
// catalog's capability declarations before serving any request.
type WireSpec struct {
	// ReasoningScope declares the verification scope this deployment's
	// reasoning traces belong to. It replaces the derived scope — provider,
	// model, and credential profile — so an operator who has verified that
	// several models or credentials decrypt and verify each other's traces
	// says so once. Unset keeps the conservative derived scope, which never
	// replays a trace across a model or an account.
	ReasoningScope string `json:"reasoning_scope,omitempty"`
	// VideoInput allows video content parts, a compatible-endpoint extension:
	// OpenAI's own schema has no video content part. Only the Chat
	// Completions surface lowers it today (as `video_url`), and only when a
	// model's capabilities declare PartVideo, so both the endpoint fact and
	// the model declaration have to hold before a request can carry one.
	VideoInput bool `json:"video_input,omitempty"`
	// ExtraBody is the deployment-level form of GenerateOptions.JSONSet: body
	// fields this driver does not model, applied to every request the
	// deployment serves. Each key is an sjson path and each value the raw
	// JSON to place there, with the same bounds and the same rejected keys.
	//
	// It is deployment configuration, not a request decision: like
	// `endpoint.headers` or `store` it never appears in the compile report,
	// and a request-level `json_set` key wins over it (a nested key merges
	// into the object this one wrote, an identical key replaces it).
	ExtraBody map[string]json.RawMessage `json:"extra_body,omitempty"`
	// Store asks the provider to retain the response server-side. Nil keeps
	// the driver default (false): FlowCraft replays context itself, and the
	// OpenAI default (true, retained for at least 30 days) is unnecessary
	// and surprising. The string "omit" leaves the field off the wire
	// entirely, for compatible endpoints that reject request fields their
	// schema does not know.
	Store *StoreSetting `json:"store,omitempty"`
	// IncludeReasoningPayload sends include: ["reasoning.encrypted_content"]
	// so reasoning traces round-trip into later context. Nil derives from
	// ReasoningChannel: true for "summary", false for "text".
	IncludeReasoningPayload *bool `json:"include_reasoning_payload,omitempty"`
	// ReasoningChannel selects the reasoning round-trip shape: "summary"
	// (default) carries summary text plus an opaque encrypted payload;
	// "text" carries plain reasoning text merged into the adjacent
	// assistant message.
	ReasoningChannel string `json:"reasoning_channel,omitempty"`
	// ReasoningSummary asks a reasoning model to emit readable summaries:
	// "auto", "concise", or "detailed". Empty sends nothing, and the
	// provider then returns the encrypted payload without summary text.
	// Responses only: Chat Completions has no summary channel.
	ReasoningSummary string `json:"reasoning_summary,omitempty"`
	// Truncation selects what the provider does when a request exceeds the
	// context window: "disabled" (default) fails the request, "auto" drops
	// the oldest items to fit. Responses only.
	Truncation string `json:"truncation,omitempty"`
	// ChatStreamOptions controls chat-completions stream transport when API
	// is "chat". Responses streams always carry usage in their terminal
	// event and never consult this setting.
	ChatStreamOptions *ChatStreamOptionsSpec `json:"chat_stream_options,omitempty"`
}

// StoreSetting is the wire policy for the provider's server-side retention
// field. It decodes from a JSON boolean (send that value) or the string
// "omit" (send nothing), so the field can be absent for endpoints whose
// schema does not know it.
type StoreSetting struct {
	// Send reports whether the field rides the request at all.
	Send bool `json:"send"`
	// Value is the boolean the request carries when Send is true.
	Value bool `json:"value"`
}

// UnmarshalJSON accepts a boolean or the string "omit".
func (s *StoreSetting) UnmarshalJSON(data []byte) error {
	var value bool
	if err := json.Unmarshal(data, &value); err == nil {
		*s = StoreSetting{Send: true, Value: value}
		return nil
	}
	var mode string
	if err := json.Unmarshal(data, &mode); err != nil || mode != "omit" {
		return fmt.Errorf(
			`wire.store must be a boolean or "omit"`,
		)
	}
	*s = StoreSetting{}
	return nil
}

// MarshalJSON renders the declared policy in its compact config form.
func (s StoreSetting) MarshalJSON() ([]byte, error) {
	if !s.Send {
		return json.Marshal("omit")
	}
	return json.Marshal(s.Value)
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
	// stream-obfuscation fields. Set false to disable stream obfuscation;
	// this keeps stream_options on the wire even when IncludeUsage is false.
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

// ModelSpec declares a model outside the built-in catalog, or a delta over a
// same-named, same-kind built-in catalog entry. Capabilities is a patch with
// field-presence semantics: every leaf it names replaces that leaf of the
// entry it overrides, and leaves it does not name are inherited — so
// redeclaring a built-in to tweak one channel keeps every other declared
// fact, including the reasoning effort map and custom embed output
// dimensions. Reasoning off needs no separate declaration: "toggle" on the
// Responses surface means this model honors reasoning.effort="none", and
// models whose endpoint cannot disable reasoning publish "always".
type ModelSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Capabilities declares the capability leaves this model changes.
	Capabilities *model.CapabilitiesPatch `json:"capabilities,omitempty"`
	// Limits declares numeric capacity limits for the model. Overriding a
	// built-in catalog entry by name keeps the catalog limit for any field
	// left nil; declaring a value replaces it.
	Limits model.ModelLimits `json:"limits,omitempty"`
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
var headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)

func (s Spec) Validate() error {
	switch s.apiMode() {
	case apiResponses, apiChat:
	default:
		return fmt.Errorf("api must be \"responses\" or \"chat\"")
	}
	if s.Wire.ChatStreamOptions != nil && s.apiMode() != apiChat {
		return fmt.Errorf("wire.chat_stream_options requires api \"chat\"")
	}
	if s.Wire.VideoInput && s.apiMode() != apiChat {
		// The Responses surface has no video content item: FlowCraft has no
		// verified lowering for one. Rejecting the fact at build time keeps a
		// deployment from publishing a capability the compiler would refuse
		// on every request.
		return fmt.Errorf(
			"wire.video_input requires api \"chat\": the responses surface has no video input lowering",
		)
	}
	if err := validateReasoningScope(s.Wire.ReasoningScope); err != nil {
		return err
	}
	if err := validateBodyFields("wire.extra_body", s.Wire.ExtraBody); err != nil {
		return err
	}
	// The deployment owns its metadata envelope field, so extra_body must not
	// write the same name on every request: two sources for one field would
	// make the wire depend on application order rather than on configuration.
	if envelope := s.requestMetadataEnvelope(); envelope != "" {
		for path := range s.Wire.ExtraBody {
			if jsonSetRoot(path) == envelope {
				return fmt.Errorf(
					"wire.extra_body %q is the request_metadata envelope field",
					path,
				)
			}
		}
	}
	// extra_body rides the generate surfaces. A declared catalog that has no
	// generate model can never use it, and a setting that can never apply is a
	// configuration error rather than a silent no-op. (builtin_declared always
	// inherits the built-in generate line-up, so it is never in that state.)
	if len(s.Wire.ExtraBody) > 0 &&
		s.catalogMode() == catalogDeclared &&
		!s.declaresGenerateModel() {
		return fmt.Errorf(
			"wire.extra_body applies to the generate surfaces; " +
				"this deployment declares no generate model",
		)
	}
	if s.Endpoint.BaseURL != "" &&
		!strings.HasPrefix(s.Endpoint.BaseURL, "https://") &&
		!strings.HasPrefix(s.Endpoint.BaseURL, "http://") {
		return fmt.Errorf("endpoint.base_url must be an http(s) URL")
	}
	if s.Endpoint.Timeout != "" {
		timeout, err := time.ParseDuration(s.Endpoint.Timeout)
		if err != nil || timeout <= 0 {
			return fmt.Errorf(
				"endpoint.timeout %q must be a positive duration (e.g. \"90s\")",
				s.Endpoint.Timeout,
			)
		}
	}
	switch s.routing() {
	case routingDirect, routingAzureDeployment:
	default:
		return fmt.Errorf("endpoint.routing must be \"azure_deployment\" or empty")
	}
	for key, value := range s.Endpoint.Query {
		if key == "" || strings.ContainsAny(key, "&=?#") {
			return fmt.Errorf("endpoint.query key %q is not a query token", key)
		}
		if strings.ContainsAny(value, "&#") {
			return fmt.Errorf(
				"endpoint.query[%q] value %q is not a query value token",
				key,
				value,
			)
		}
	}
	for name := range s.Endpoint.Headers {
		if !headerNamePattern.MatchString(name) {
			return fmt.Errorf("endpoint.headers key %q is not a header name", name)
		}
		// Static headers must not carry credentials: secret material rides
		// the profile's api_key so core resolves and redacts it.
		if strings.EqualFold(name, "authorization") ||
			strings.EqualFold(name, "api-key") {
			return fmt.Errorf(
				"endpoint.headers must not carry credentials; %q belongs in auth",
				name,
			)
		}
	}
	switch s.authScheme() {
	case authBearer, authHeader, authNone:
	default:
		return fmt.Errorf(
			"auth.scheme must be \"bearer\", \"header\", or \"none\"",
		)
	}
	switch {
	case s.authScheme() == authHeader && !headerNamePattern.MatchString(s.Auth.Header):
		return fmt.Errorf(
			"auth.header %q is required when auth.scheme is \"header\"",
			s.Auth.Header,
		)
	case s.authScheme() == authBearer && s.Auth.Header != "":
		return fmt.Errorf("auth.header requires auth.scheme \"header\"")
	case s.authScheme() == authNone && s.Auth.Header != "":
		return fmt.Errorf("auth.header requires auth.scheme \"header\"")
	}
	if s.routing() == routingAzureDeployment {
		// Azure carries the key in Api-Key and always needs api-version, so
		// this endpoint mode owns both instead of letting a second config
		// path describe the same contract.
		if s.Auth != (AuthSpec{}) && s.authScheme() != authHeader {
			return fmt.Errorf(
				"endpoint.routing \"azure_deployment\" requires auth.scheme \"header\"",
			)
		}
		if version, ok := s.Endpoint.Query["api-version"]; ok && version == "" {
			return fmt.Errorf("endpoint.query api-version must not be empty")
		}
	}
	switch s.reasoningChannel() {
	case channelSummary, channelText:
	default:
		return fmt.Errorf("wire.reasoning_channel must be \"summary\" or \"text\"")
	}
	switch s.reasoningSummaryPolicy() {
	case "", summaryAuto, summaryConcise, summaryDetailed:
	default:
		return fmt.Errorf(
			"wire.reasoning_summary must be \"auto\", \"concise\", or \"detailed\"",
		)
	}
	switch s.truncation() {
	case "", truncationAuto, truncationDisabled:
	default:
		return fmt.Errorf("wire.truncation must be \"auto\" or \"disabled\"")
	}
	if s.truncation() != "" && s.apiMode() != apiResponses {
		return fmt.Errorf("wire.truncation requires api \"responses\"")
	}
	if s.reasoningSummaryPolicy() != "" && s.apiMode() != apiResponses {
		return fmt.Errorf("wire.reasoning_summary requires api \"responses\"")
	}
	if s.reasoningChannel() == channelText &&
		s.Wire.IncludeReasoningPayload != nil &&
		*s.Wire.IncludeReasoningPayload {
		return fmt.Errorf(
			"wire.include_reasoning_payload requires wire.reasoning_channel \"summary\"",
		)
	}
	switch s.catalogMode() {
	case catalogBuiltinDeclared, catalogDeclared:
	default:
		return fmt.Errorf("catalog must be \"builtin_declared\" or \"declared\"")
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

func (m ModelSpec) Validate() error {
	if !modelNamePattern.MatchString(m.Name) {
		return fmt.Errorf("invalid model name %q", m.Name)
	}
	kind := modelKind(m.Kind)
	switch kind {
	case kindGenerate, kindEmbed, kindImage, kindTTS:
	default:
		return fmt.Errorf("model %q has unknown kind %q", m.Name, m.Kind)
	}
	if m.Capabilities != nil &&
		m.Capabilities.CustomEmbedDimensions != nil &&
		*m.Capabilities.CustomEmbedDimensions &&
		kind != kindEmbed {
		return fmt.Errorf(
			"model %q sets custom_embed_dimensions on kind %q",
			m.Name,
			m.Kind,
		)
	}
	if err := m.Capabilities.Validate(); err != nil {
		return err
	}
	return m.Limits.Validate()
}

// declaresGenerateModel reports whether the spec declares at least one
// generate-kind model, i.e. whether the deployment can serve the surfaces
// wire.extra_body applies to.
func (s Spec) declaresGenerateModel() bool {
	for _, model := range s.Models {
		if modelKind(model.Kind) == kindGenerate {
			return true
		}
	}
	return false
}

// maxReasoningScopeLen bounds the declared scope token. The token lands in a
// compile-report reason, so it stays short and printable.
const maxReasoningScopeLen = 128

// validateReasoningScope checks the declared verification scope: opaque to
// the driver, but never surrounding-whitespace sensitive (the comparison is
// exact), never a control character, and never unbounded.
func validateReasoningScope(scope string) error {
	if scope == "" {
		return nil
	}
	if strings.TrimSpace(scope) != scope {
		return fmt.Errorf("wire.reasoning_scope must not have surrounding whitespace")
	}
	if len(scope) > maxReasoningScopeLen {
		return fmt.Errorf(
			"wire.reasoning_scope is %d bytes, at most %d are allowed",
			len(scope), maxReasoningScopeLen,
		)
	}
	for _, char := range scope {
		if unicode.IsControl(char) {
			return fmt.Errorf(
				"wire.reasoning_scope must not contain control characters")
		}
	}
	return nil
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
