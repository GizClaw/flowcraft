package bytedance

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
)

// clients bundles the Ark SDK client and the deployment-wide request options.
type clients struct {
	ark               *arkruntime.Client
	arkRequestOptions []arkruntime.RequestOption
	endpoints         map[string]string
	apiKey            string
	baseURL           string
	httpClient        *http.Client
}

func newClients(settings Settings) (*clients, error) {
	built := &clients{
		endpoints:  settings.Endpoints,
		apiKey:     settings.APIKey,
		baseURL:    settings.BaseURL,
		httpClient: settings.HTTPClient,
	}
	if built.httpClient == nil {
		built.httpClient = &http.Client{Timeout: settings.Timeout}
	}
	options := []arkruntime.ConfigOption{
		arkruntime.WithHTTPClient(built.httpClient),
		arkruntime.WithRetryTimes(settings.Retries),
	}
	if settings.BaseURL != "" {
		options = append(options, arkruntime.WithBaseUrl(settings.BaseURL))
	}
	if settings.Region != "" {
		options = append(options, arkruntime.WithRegion(settings.Region))
	}
	if len(settings.Headers) > 0 {
		built.arkRequestOptions = append(built.arkRequestOptions,
			arkruntime.WithCustomHeaders(settings.Headers))
	}
	for key, value := range settings.Query {
		built.arkRequestOptions = append(built.arkRequestOptions,
			arkruntime.WithQuery(key, value))
	}
	if settings.Project != "" {
		built.arkRequestOptions = append(built.arkRequestOptions,
			arkruntime.WithProjectName(settings.Project))
	}
	built.ark = arkruntime.NewClientWithApiKey(strings.TrimSpace(settings.APIKey), options...)
	return built, nil
}

func (c *clients) endpoint(name string) string {
	if endpoint, ok := c.endpoints[name]; ok {
		return endpoint
	}
	return name
}

func (c *clients) requireArk() (*arkruntime.Client, error) {
	if c.ark == nil {
		return nil, fmt.Errorf("bytedance: no Ark client")
	}
	return c.ark, nil
}
