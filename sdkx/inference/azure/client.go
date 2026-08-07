package azure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/config"

	openaigo "github.com/openai/openai-go"
	"github.com/openai/openai-go/azure"
	"github.com/openai/openai-go/option"
)

// SecretAPIKey is the Azure OpenAI resource key secret id.
const SecretAPIKey = "api_key"

// profileMaterial is the resolved credential set for one profile.
type profileMaterial struct {
	apiKey string
}

func newProfileMaterial(profile config.ResolvedProfile) (profileMaterial, error) {
	material := profileMaterial{}
	for id, secret := range profile.Secrets {
		switch id {
		case SecretAPIKey:
			material.apiKey = strings.TrimSpace(string(secret.Bytes()))
		default:
			return profileMaterial{}, fmt.Errorf(
				"azure: profile %q carries unknown secret %q (supported: api_key)",
				profile.ID,
				id,
			)
		}
	}
	if material.apiKey == "" {
		return profileMaterial{}, fmt.Errorf(
			"azure: profile %q is missing the required api_key secret",
			profile.ID,
		)
	}
	return material, nil
}

// clients bundles the service handles one profile needs. Every operation
// surface shares the single typed SDK client.
type clients struct {
	api openaigo.Client
}

func (m profileMaterial) newClients(spec Spec) *clients {
	version := spec.APIVersion
	if version == "" {
		version = DefaultAPIVersion
	}
	options := []option.RequestOption{
		azure.WithEndpoint(strings.TrimSuffix(spec.Endpoint, "/"), version),
		azure.WithAPIKey(m.apiKey),
		option.WithMiddleware(deploymentRouteMiddleware),
	}
	if spec.HTTPRetries != nil {
		options = append(options, sdkMaxRetriesOption(*spec.HTTPRetries))
	}
	return &clients{
		api: openaigo.NewClient(options...),
	}
}

// sdkMaxRetriesOption converts a total-attempt budget (including the first)
// into the openai-go retry-count option.
func sdkMaxRetriesOption(total int) option.RequestOption {
	if total <= 1 {
		return option.WithMaxRetries(0)
	}
	return option.WithMaxRetries(total - 1)
}

// deploymentRouteMiddleware scopes the Responses API route to its
// deployment. The pinned SDK's azure middleware predates the Responses API
// and only rewrites chat completions, embeddings, speech, images, and
// transcriptions; /openai/responses would otherwise hit the deployment-less
// path and 404. The rewrite mirrors the SDK's own getJSONRoute.
func deploymentRouteMiddleware(
	r *http.Request,
	next option.MiddlewareNext,
) (*http.Response, error) {
	if r.URL.Path != "/openai/responses" || r.Body == nil {
		return next(r)
	}
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(payload))
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}
	if envelope.Model != "" {
		r.URL.Path = "/openai/deployments/" +
			url.PathEscape(envelope.Model) + "/responses"
	}
	return next(r)
}
