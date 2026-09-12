package openai

import (
	"strings"
	"time"

	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
)

// This file turns the EndpointSpec and AuthSpec blocks into a configured
// client: where requests go, how paths are rewritten, and how the profile's
// key rides the wire. None of it changes how a request is compiled.

// DefaultBaseURL is the endpoint used when endpoint.base_url is unset.
const DefaultBaseURL = "https://api.openai.com/v1"

// DefaultAzureAPIVersion is the api-version used when an Azure deployment
// endpoint does not name one.
const DefaultAzureAPIVersion = "2025-04-01-preview"

// endpointRouting selects how request paths reach the endpoint.
type endpointRouting string

const (
	// routingDirect posts to the plain OpenAI routes.
	routingDirect endpointRouting = ""
	// routingAzureDeployment rewrites data-plane routes to
	// /openai/deployments/{model}/... and adds the api-version query.
	routingAzureDeployment endpointRouting = "azure_deployment"
)

// authScheme selects the credential transport.
type authScheme string

const (
	authBearer authScheme = "bearer"
	authHeader authScheme = "header"
	// authNone authenticates nothing: local or in-cluster gateways often
	// require no credential at all.
	authNone authScheme = "none"
)

// routing returns the normalized endpoint routing.
func (s Spec) routing() endpointRouting {
	return endpointRouting(s.Endpoint.Routing)
}

// authScheme returns the normalized credential transport.
func (s Spec) authScheme() authScheme {
	if s.Auth.Scheme == "" {
		return authBearer
	}
	return authScheme(s.Auth.Scheme)
}

// baseURL returns the resolved endpoint URL.
func (s Spec) baseURL() string {
	if s.Endpoint.BaseURL != "" {
		return s.Endpoint.BaseURL
	}
	return DefaultBaseURL
}

// azureAPIVersion returns the api-version an Azure deployment endpoint uses.
func (s Spec) azureAPIVersion() string {
	if version := s.Endpoint.Query["api-version"]; version != "" {
		return version
	}
	return DefaultAzureAPIVersion
}

// endpointTimeout returns the per-request timeout, or zero for the SDK
// default.
func (s Spec) endpointTimeout() time.Duration {
	if s.Endpoint.Timeout == "" {
		return 0
	}
	timeout, err := time.ParseDuration(s.Endpoint.Timeout)
	if err != nil || timeout <= 0 {
		return 0
	}
	return timeout
}

// requestOptions assembles the transport for one resolved credential: base
// URL, auth, query, headers, retries, and timeout. The credential is empty
// for endpoints configured with auth.scheme "none".
func (s Spec) requestOptions(apiKey string) []option.RequestOption {
	options := []option.RequestOption{}
	switch s.routing() {
	case routingAzureDeployment:
		// The SDK's Azure mode owns the parts that are easy to get wrong:
		// the Api-Key header, the api-version query, and the
		// /openai/deployments/{model}/... path rewriting for both JSON and
		// multipart routes.
		options = append(options,
			azure.WithEndpoint(
				strings.TrimSuffix(s.baseURL(), "/"),
				s.azureAPIVersion(),
			),
			azure.WithAPIKey(apiKey),
		)
	default:
		switch s.authScheme() {
		case authNone:
			// No credential: drop the bearer header the SDK installs by
			// default so nothing rides the wire.
			options = append(options, option.WithHeaderDel("Authorization"))
		case authHeader:
			// A named header carries the key: drop the bearer header the
			// SDK installs by default so credentials never ride twice.
			options = append(options,
				option.WithHeaderDel("Authorization"),
				option.WithHeader(s.Auth.Header, apiKey),
			)
		default:
			options = append(options, option.WithAPIKey(apiKey))
		}
		options = append(options, option.WithBaseURL(s.baseURL()))
	}
	if s.Endpoint.Organization != "" {
		options = append(options, option.WithOrganization(s.Endpoint.Organization))
	}
	if s.Endpoint.Project != "" {
		options = append(options, option.WithProject(s.Endpoint.Project))
	}
	for name, value := range s.Endpoint.Headers {
		options = append(options, option.WithHeader(name, value))
	}
	for name, value := range s.Endpoint.Query {
		if s.routing() == routingAzureDeployment && name == "api-version" {
			// azure.WithEndpoint already installs the version.
			continue
		}
		options = append(options, option.WithQueryAdd(name, value))
	}
	if s.HTTPRetries != nil {
		options = append(options,
			option.WithMaxRetries(sdkMaxRetries(int(*s.HTTPRetries))))
	}
	if timeout := s.endpointTimeout(); timeout > 0 {
		options = append(options, option.WithRequestTimeout(timeout))
	}
	return options
}

// sdkMaxRetries converts a total-attempt budget (including the first) into
// the SDK's retry-count option.
func sdkMaxRetries(total int) int {
	if total <= 1 {
		return 0
	}
	return total - 1
}
