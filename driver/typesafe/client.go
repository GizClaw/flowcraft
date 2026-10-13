package typesafe

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils"
)

// The two headers the transport owns. endpoint.headers refuses them for the
// same reason: a deployment-supplied Authorization would ride alongside the
// profile's key, and a foreign Content-Type could make the route reject a body
// this driver encoded as JSON.
const (
	headerAuthorization = "Authorization"
	headerContentType   = "Content-Type"
)

// profileMaterial is one profile's credential and the resolver it was resolved
// through. Secrets resolve lazily — at the first call that needs them — so a
// deployment is built before its secret store is necessarily reachable.
type profileMaterial struct {
	apiKey   resource.Secret
	resolver *resource.SecretResolver
}

// clients carries the handles one profile opens drivers with. The whole wire is
// one JSON route, so an HTTP client rooted at the profile's API root, the key
// it authenticates with, and the deployment's static headers are the complete
// state.
type clients struct {
	http    *http.Client
	key     string
	base    string
	headers map[string]string
}

// profileSpec is the reserved profile-level settings document. This driver has
// no per-profile transport fact: TypeSafe keys are not region-bound the way a
// DashScope key is, and base_url is a provider-wide fact.
type profileSpec struct{}

func newProfileMaterial(
	ctx context.Context,
	profile ProfileSettings,
	secrets *resource.SecretResolver,
) (profileMaterial, error) {
	// The reserved profile spec is decoded strictly as an empty document, so
	// a per-profile setting this driver does not implement fails the build
	// instead of being silently ignored.
	if _, err := resource.DecodeTyped[profileSpec](ctx, profile.Spec); err != nil {
		return profileMaterial{}, fmt.Errorf("profile %q spec: %w", profile.ID, err)
	}
	names := make([]string, 0, len(profile.Secrets))
	for name := range profile.Secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if name != SecretAPIKey {
			return profileMaterial{}, fmt.Errorf(
				"profile %q carries unknown secret %q", profile.ID, name)
		}
	}
	material := profileMaterial{resolver: secrets}
	if secret, ok := profile.Secrets[SecretAPIKey]; ok {
		material.apiKey = secret
	}
	return material, nil
}

// newClients resolves the profile's credential and roots one HTTP client at the
// deployment's API root.
func (m profileMaterial) newClients(ctx context.Context, spec Spec) (*clients, error) {
	apiKey, err := m.apiKey.Resolve(ctx, m.resolver)
	if err != nil {
		return nil, errdefs.Validationf("typesafe profile: resolve api_key: %v", err)
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errdefs.Validationf(
			"typesafe profile resolves no api_key secret")
	}
	// The httpkit client owns the hardened transport, its protocol and
	// pooling defaults, and the retry transport that stamps the attempt count
	// the error path reads back.
	options := make([]utils.Option, 0, 1)
	if spec.HTTPRetries != nil {
		options = append(options, utils.WithRetryAttempts(int(*spec.HTTPRetries)))
	}
	headers := make(map[string]string, len(spec.Endpoint.Headers))
	for name, value := range spec.Endpoint.Headers {
		headers[name] = value
	}
	return &clients{
		http:    utils.NewHttpClient(options...),
		key:     apiKey,
		base:    spec.baseURL(),
		headers: headers,
	}, nil
}
