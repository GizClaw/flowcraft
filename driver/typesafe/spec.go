package typesafe

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
)

// SecretAPIKey is the provider-owned secret name for the TypeSafe API key.
const SecretAPIKey = "api_key"

// DefaultBaseURL is the API root used when the deployment does not override
// endpoint.base_url. It carries the version segment the route is served under,
// so the driver appends only the route name (/systemone) and a deployment
// behind a compatible proxy points base_url at that proxy's version root. The
// route is published at POST {root}/systemone, and the vendor's OpenAPI
// document (at {origin}/openapi.json) names it /v1/systemone.
const DefaultBaseURL = "https://api.typesafe.ai/v1"

// DefaultModel is the model alias the vendor's own examples name. It is not a
// line-up: the driver serves the models a deployment declares, and a
// declaration may pin a versioned id (jev-1.13.0) instead — the alias is
// resolved by the endpoint, which answers with the versioned id it ran — so
// this constant exists only for whatever a deployment or a test wants to name
// without restating the alias. Aliases resolving to versioned ids is the
// vendor's own documented behavior (docs.typesafe.ai/models), and GET
// /v1/models is the route that lists them.
const DefaultModel = "jev-latest"

// Spec is the provider-level configuration surface. It is credential-free:
// secrets resolve per profile and never appear here.
type Spec struct {
	// Endpoint carries the transport facts: where the route lives and which
	// static headers ride every request.
	Endpoint EndpointSpec `json:"endpoint,omitempty"`
	// HTTPRetries bounds wire-level HTTP retries, counting the first attempt.
	// Zero disables transport retries so the route Router owns the full
	// budget; nil keeps the httpkit default (three attempts). The vendor
	// publishes no retry policy of its own, so the choice belongs to the
	// deployment.
	HTTPRetries *resource.Int `json:"http_retries,omitempty"`
	// Models declares the line-up this deployment serves. It is the only
	// source of models: the driver ships none, so an undeclared model is not
	// served.
	Models []ModelSpec `json:"models,omitempty"`
}

// EndpointSpec is the transport half of the provider Spec.
type EndpointSpec struct {
	// BaseURL is the API root the route is appended to. It is the
	// versioned root (https://api.typesafe.ai/v1 by default), not the bare
	// host: a compatible proxy that serves the wire under its own version
	// segment declares that root here and nothing else changes.
	BaseURL string `json:"base_url,omitempty"`
	// Headers are static headers sent with every request — a gateway's
	// routing or tenancy header, for instance. They are a deployment fact,
	// not a credential: Authorization and Content-Type are owned by the
	// transport and are refused here.
	Headers map[string]string `json:"headers,omitempty"`
}

// ModelSpec declares one model this deployment serves. The driver ships no
// line-up, so the declaration is complete: what it states is what the model
// promises, and a leaf it leaves out is undeclared rather than inherited.
type ModelSpec struct {
	Name string `json:"name"`
	// Kind selects the family that serves the model. This driver has one:
	// "decision".
	Kind string `json:"kind"`
	// Capabilities declares what the model accepts and produces. A decision
	// model's only input kind is text — the wire carries one state value —
	// and it declares no output kind at all.
	Capabilities model.ModelCapabilities `json:"capabilities,omitempty"`
	// Limits declares the model's numeric capacity limits. Undeclared leaves
	// claim no bound rather than a zero one.
	Limits model.ModelLimits `json:"limits,omitempty"`
}

// modelNamePattern bounds a declared model name. Both spellings a deployment
// may put on the wire's model field pass: the vendor's alias (jev-latest) and
// the versioned id it reports back (jev-1.13.0).
var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// headerNamePattern is the HTTP token grammar for a header name. Declared
// headers are a deployment document, so a name that is not a token fails the
// build instead of reaching net/http, which would refuse it at request time and
// turn a configuration typo into a per-call failure.
var headerNamePattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9!#$%&'*+.^_`|~-]*$")

// Validate checks the provider spec for structural sanity.
func (s Spec) Validate() error {
	if err := validateBaseURL(s.Endpoint.BaseURL); err != nil {
		return err
	}
	if err := validateHeaders(s.Endpoint.Headers); err != nil {
		return err
	}
	if s.HTTPRetries != nil && *s.HTTPRetries < 0 {
		return fmt.Errorf("http_retries must not be negative")
	}
	seen := make(map[string]struct{}, len(s.Models))
	for _, declared := range s.Models {
		if err := declared.Validate(); err != nil {
			return fmt.Errorf("model %q: %w", declared.Name, err)
		}
		if _, duplicate := seen[declared.Name]; duplicate {
			return fmt.Errorf("duplicate model declaration %q", declared.Name)
		}
		seen[declared.Name] = struct{}{}
	}
	return nil
}

// Validate checks one model declaration against the vocabulary and the
// declaration's own structural contract. The family contract — which content
// kinds this wire can actually carry — is checked separately, in
// validateDeclaration, because it is a fact about the wire rather than about
// the declaration's shape.
func (m ModelSpec) Validate() error {
	if !modelNamePattern.MatchString(m.Name) {
		return fmt.Errorf("invalid model name %q", m.Name)
	}
	if modelKind(m.Kind) != kindDecision {
		return fmt.Errorf(
			"model %q has unknown kind %q: this driver serves %q",
			m.Name,
			m.Kind,
			kindDecision,
		)
	}
	if err := m.Capabilities.Validate(); err != nil {
		return err
	}
	return m.Limits.Validate()
}

// baseURL resolves the API root the route is appended to.
func (s Spec) baseURL() string {
	if s.Endpoint.BaseURL != "" {
		return strings.TrimRight(s.Endpoint.BaseURL, "/")
	}
	return DefaultBaseURL
}

// validateBaseURL rejects an endpoint root the driver cannot send a request to.
// The route is appended to it as a path, so the value has to be an absolute
// http(s) URL with a host and without a query or fragment — a base URL that
// carries either would put the route inside the query instead of after it.
//
// Credentials in the URL are refused as well: a provider document is
// credential-free by contract (secrets resolve per profile), so userinfo here
// would be a secret in a file that is not the secret store, sent on every call.
//
// A value that is only almost right fails here, while the deployment document
// is read, rather than at request time — where the same typo would answer every
// call with a failure that never reached the route.
func validateBaseURL(value string) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("endpoint.base_url is not a URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf(
			"endpoint.base_url must be an http(s) URL, got scheme %q",
			parsed.Scheme,
		)
	}
	if parsed.Host == "" {
		return fmt.Errorf("endpoint.base_url has no host")
	}
	if parsed.User != nil {
		return fmt.Errorf(
			"endpoint.base_url carries credentials: secrets belong to a profile")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf(
			"endpoint.base_url carries a query or fragment: the route is appended " +
				"to the path")
	}
	return nil
}

// validateHeaders checks the deployment's static headers. Names are visited in
// sorted order so a document with several problems always reports the same one.
func validateHeaders(headers map[string]string) error {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !headerNamePattern.MatchString(name) {
			return fmt.Errorf("endpoint.headers: %q is not a header name", name)
		}
		switch strings.ToLower(name) {
		case "authorization", "content-type":
			return fmt.Errorf(
				"endpoint.headers: %q is set by the driver for every request", name)
		}
		if strings.ContainsAny(headers[name], "\r\n\x00") {
			return fmt.Errorf(
				"endpoint.headers: %q carries a control character", name)
		}
	}
	return nil
}

// validateDeclaration enforces the family contract for a kind "decision" model:
// the wire carries one text state value, so text is the only input kind a
// declaration may name, and a decision model produces no content kind at all,
// so a declared output is a capability the runtime would route on. Images are
// not a System One concept — the vendor sends non-text material through state,
// pre-processed by the caller — which is why the input refusal names the kind
// rather than dropping it.
func validateDeclaration(declared ModelSpec) error {
	for _, input := range declared.Capabilities.Inputs {
		if input != message.PartText {
			return fmt.Errorf(
				"the System One wire carries one text state value, not %s input; "+
					"drop it from capabilities.inputs",
				input,
			)
		}
	}
	if len(declared.Capabilities.Outputs) != 0 {
		return fmt.Errorf("decision family declares no generate output")
	}
	return nil
}

// decodeSpec strictly decodes the provider Spec: unknown fields fail the build
// so a typo cannot silently drop policy, and the vendor's own facts are
// validated on the way in.
func decodeSpec(ctx context.Context, raw []byte) (Spec, error) {
	spec, err := resource.DecodeTyped[Spec](ctx, raw)
	if err != nil {
		return Spec{}, fmt.Errorf("typesafe spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, fmt.Errorf("typesafe spec: %w", err)
	}
	return spec, nil
}
