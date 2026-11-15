package bytedance

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/GizClaw/flowcraft/driver/internal/httpx"
)

// ArkResponseLimit bounds one JSON answer. A task query is small, but an embed
// batch over a long input is not, so the ceiling is generous and finite.
const ArkResponseLimit int64 = 32 << 20

// clients bundles the wire clients and the deployment-wide request options.
//
// Two handles, because one host must not see the other's credential: `api`
// carries the account key, the project header and the deployment's query
// parameters; `files` fetches the media URL a finished task names, which is
// signed on its own host.
type clients struct {
	api       *httpx.Client
	files     *httpx.Client
	query     url.Values
	endpoints map[string]string
	apiKey    string
	baseURL   string
}

func newClients(settings Settings) (*clients, error) {
	built := &clients{
		endpoints: settings.Endpoints,
		apiKey:    settings.APIKey,
		baseURL:   settings.BaseURL,
		query:     url.Values{},
	}
	transport := settings.HTTPClient
	if transport == nil {
		transport = &http.Client{Timeout: settings.Timeout}
	}
	headers := make(map[string]string, len(settings.Headers)+1)
	for key, value := range settings.Headers {
		headers[key] = value
	}
	if settings.Project != "" {
		// The platform names the project on the request, not in the body.
		headers["X-Project-Name"] = settings.Project
	}
	for key, value := range settings.Query {
		built.query.Set(key, value)
	}
	built.api = httpx.New(httpx.Config{
		BaseURL: settings.BaseURL,
		APIKey:  strings.TrimSpace(settings.APIKey),
		Headers: headers,
		HTTP:    transport,
		Limit:   ArkResponseLimit,
	})
	built.files = httpx.New(httpx.Config{HTTP: transport})

	return built, nil
}

func (c *clients) endpoint(name string) string {
	if endpoint, ok := c.endpoints[name]; ok {
		return endpoint
	}
	return name
}
