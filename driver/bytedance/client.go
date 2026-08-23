package bytedance

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/utils"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
)

// defaultResponseHeaderTimeout bounds how long Ark may take before
// response headers arrive; HTTP/1.1 + this timeout keeps a stalled
// request from wedging the shared connection.
const defaultResponseHeaderTimeout = 5 * time.Minute

// defaultClientTimeout is the whole-request budget (mirrors the SDK
// default of 10m).
const defaultClientTimeout = 10 * time.Minute

// defaultArkBaseURL mirrors the SDK's default when the profile does not
// override it; the raw images path derives its URL from the same value.
const defaultArkBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

// profileMaterial is one credential profile after secret resolution: the
// decoded profile Spec plus the secret values this provider recognizes.
type profileMaterial struct {
	spec   ProfileSpec
	apiKey string
}

// clients bundles the service clients one profile needs. ark is nil when the
// profile has no Ark credential. Drivers check the client they need at open
// time.
type clients struct {
	ark *arkruntime.Client
	// endpoints binds model names to this account's deployment addresses
	// (ProfileSpec.Endpoints); empty maps resolve to the catalog name.
	endpoints map[string]string
	// Raw request support: the pinned SDK cannot encode every official
	// parameter (image layer_decomposition/background), so the image
	// transport falls back to a direct POST carrying the same credentials,
	// base URL, and HTTP client.
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// endpoint resolves the wire address for one catalog model within this
// profile's account: the mapped endpoint when present, the catalog name
// otherwise.
func (c *clients) endpoint(name string) string {
	if endpoint, ok := c.endpoints[name]; ok {
		return endpoint
	}
	return name
}

func newProfileMaterial(profile ProfileSettings) (profileMaterial, error) {
	spec, err := decodeProfileSpec(profile.Spec)
	if err != nil {
		return profileMaterial{}, err
	}
	material := profileMaterial{spec: spec}
	for name := range profile.Secrets {
		switch name {
		case SecretAPIKey:
		default:
			return profileMaterial{}, fmt.Errorf(
				"bytedance profile %q carries unknown secret %q",
				profile.ID,
				name,
			)
		}
	}
	if secret, ok := profile.Secrets[SecretAPIKey]; ok {
		material.apiKey = strings.TrimSpace(secret)
	}
	if material.apiKey == "" {
		return profileMaterial{}, fmt.Errorf(
			"bytedance profile %q needs an Ark credential (%q)",
			profile.ID,
			SecretAPIKey,
		)
	}
	return material, nil
}

// newClients builds the service clients for one profile.
func (m profileMaterial) newClients(spec Spec) *clients {
	built := &clients{endpoints: m.spec.Endpoints}
	built.apiKey = m.apiKey
	built.baseURL = spec.BaseURL
	if built.baseURL == "" {
		built.baseURL = defaultArkBaseURL
	} else {
		built.baseURL = strings.TrimSuffix(built.baseURL, "/")
	}
	options := []arkruntime.ConfigOption{}
	httpOptions := []utils.Option{
		utils.WithHTTP2(),
		utils.WithTimeout(defaultClientTimeout),
		utils.WithResponseHeaderTimeout(defaultResponseHeaderTimeout),
	}
	if spec.HTTPRetries != nil {
		httpOptions = append(httpOptions,
			utils.WithRetryAttempts(int(*spec.HTTPRetries)))
	}
	httpClient := utils.NewHttpClient(httpOptions...)
	built.httpClient = httpClient
	if spec.BaseURL != "" {
		options = append(options, arkruntime.WithBaseUrl(spec.BaseURL))
	}
	if spec.Region != "" {
		options = append(options, arkruntime.WithRegion(spec.Region))
	}
	options = append(options,
		arkruntime.WithHTTPClient(httpClient),
		// SDK-internal retries are disabled; the retry transport above owns
		// replayable transient failures so attempts do not multiply.
		arkruntime.WithRetryTimes(0),
	)
	if m.apiKey != "" {
		built.ark = arkruntime.NewClientWithApiKey(m.apiKey, options...)
	}
	return built
}

// requireArk returns the Ark client or a profile-scoped error.
func (c *clients) requireArk(profile string) (*arkruntime.Client, error) {
	if c.ark == nil {
		return nil, fmt.Errorf(
			"bytedance profile %q has no Ark credential (%q)",
			profile,
			SecretAPIKey,
		)
	}
	return c.ark, nil
}
