package bytedance

import (
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/inference/config"
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit"

	doubaospeech "github.com/GizClaw/doubao-speech-go"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
)

// Ark authentication modes. Images and content-generation tasks hard-fail
// under AK/SK in the SDK (ErrAKSKNotSupported), so the mode is recorded on
// clients and those drivers reject AK/SK profiles at open time.
const (
	arkAuthAPIKey = "api_key"
	arkAuthAKSK   = "aksk"

	// defaultResponseHeaderTimeout bounds how long Ark may take before
	// response headers arrive; HTTP/1.1 + this timeout keeps a stalled
	// request from wedging the shared connection.
	defaultResponseHeaderTimeout = 5 * time.Minute
	// defaultClientTimeout is the whole-request budget (mirrors the SDK
	// default of 10m).
	defaultClientTimeout = 10 * time.Minute
)

// profileMaterial is one credential profile after secret resolution: the
// decoded profile Spec plus the secret values this provider recognizes.
type profileMaterial struct {
	spec      ProfileSpec
	apiKey    string
	accessKey string
	secretKey string
	speech    string // speech API key override; empty means use apiKey
}

// clients bundles the service clients one profile needs. ark is nil when the
// profile has no Ark credential; speech is nil when no speech credential/app
// ID is available. Drivers check the client they need at open time.
type clients struct {
	ark     *arkruntime.Client
	arkAuth string
	speech  *doubaospeech.Client
	// endpoints binds model names to this account's deployment addresses
	// (ProfileSpec.Endpoints); empty maps resolve to the catalog name.
	endpoints map[string]string
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

func newProfileMaterial(profile config.ResolvedProfile) (profileMaterial, error) {
	spec, err := decodeProfileSpec(profile.Spec)
	if err != nil {
		return profileMaterial{}, err
	}
	material := profileMaterial{spec: spec}
	for name := range profile.Secrets {
		switch name {
		case SecretAPIKey, SecretSpeechAPIKey, SecretAccessKey, SecretSecretKey:
		default:
			return profileMaterial{}, fmt.Errorf(
				"bytedance profile %q carries unknown secret %q",
				profile.ID,
				name,
			)
		}
	}
	if secret, ok := profile.Secrets[SecretAPIKey]; ok {
		material.apiKey = secretString(secret)
	}
	if secret, ok := profile.Secrets[SecretSpeechAPIKey]; ok {
		material.speech = secretString(secret)
	}
	if secret, ok := profile.Secrets[SecretAccessKey]; ok {
		material.accessKey = secretString(secret)
	}
	if secret, ok := profile.Secrets[SecretSecretKey]; ok {
		material.secretKey = secretString(secret)
	}
	if (material.accessKey == "") != (material.secretKey == "") {
		return profileMaterial{}, fmt.Errorf(
			"bytedance profile %q must pair %q with %q",
			profile.ID,
			SecretAccessKey,
			SecretSecretKey,
		)
	}
	if material.apiKey != "" && material.accessKey != "" {
		return profileMaterial{}, fmt.Errorf(
			"bytedance profile %q mixes %q with AK/SK; pick one Ark authentication",
			profile.ID,
			SecretAPIKey,
		)
	}
	if material.apiKey == "" && material.accessKey == "" && material.speech == "" {
		return profileMaterial{}, fmt.Errorf(
			"bytedance profile %q needs an Ark credential (%q or %q+%q) or %q",
			profile.ID,
			SecretAPIKey,
			SecretAccessKey,
			SecretSecretKey,
			SecretSpeechAPIKey,
		)
	}
	return material, nil
}

// secretString converts a secret payload to a single-line credential value.
func secretString(secret config.Secret) string {
	return strings.TrimSpace(string(secret.Bytes()))
}

// newClients builds the service clients for one profile. Speech clients
// require the profile's app_id; profiles without one get no speech client,
// and speech drivers fail with a clear error when opened.
func (m profileMaterial) newClients(spec Spec) *clients {
	built := &clients{endpoints: m.spec.Endpoints}
	options := []arkruntime.ConfigOption{}
	httpOptions := []httpkit.Option{
		httpkit.WithHTTP2(),
		httpkit.WithTimeout(defaultClientTimeout),
		httpkit.WithResponseHeaderTimeout(defaultResponseHeaderTimeout),
	}
	if spec.HTTPRetries != nil {
		httpOptions = append(httpOptions, httpkit.WithRetryAttempts(*spec.HTTPRetries))
	}
	if spec.BaseURL != "" {
		options = append(options, arkruntime.WithBaseUrl(spec.BaseURL))
	}
	if spec.Region != "" {
		options = append(options, arkruntime.WithRegion(spec.Region))
	}
	options = append(options,
		arkruntime.WithHTTPClient(httpkit.NewClient(httpOptions...)),
		// SDK-internal retries are disabled; the retry transport above owns
		// replayable transient failures so attempts do not multiply.
		arkruntime.WithRetryTimes(0),
	)
	switch {
	case m.apiKey != "":
		built.ark = arkruntime.NewClientWithApiKey(m.apiKey, options...)
		built.arkAuth = arkAuthAPIKey
	case m.accessKey != "":
		built.ark = arkruntime.NewClientWithAkSk(m.accessKey, m.secretKey, options...)
		built.arkAuth = arkAuthAKSK
	}
	speechKey := m.speech
	if speechKey == "" {
		speechKey = m.apiKey
	}
	if speechKey != "" && m.spec.AppID != "" {
		options := []doubaospeech.Option{doubaospeech.WithAPIKey(speechKey)}
		// The speech SDK shares the httpkit retry budget with the Ark
		// client so http_retries governs both surfaces.
		options = append(options, doubaospeech.WithHTTPClient(
			httpkit.NewClient(httpOptions...),
		))
		if spec.SpeechBaseURL != "" {
			options = append(options, doubaospeech.WithBaseURL(spec.SpeechBaseURL))
		}
		if spec.SpeechWebSocketURL != "" {
			options = append(options, doubaospeech.WithWebSocketURL(spec.SpeechWebSocketURL))
		}
		built.speech = doubaospeech.NewClient(m.spec.AppID, options...)
	}
	return built
}

// requireArk returns the Ark client or a profile-scoped error.
func (c *clients) requireArk(profile string) (*arkruntime.Client, error) {
	if c.ark == nil {
		return nil, fmt.Errorf(
			"bytedance profile %q has no Ark credential (%q or %q+%q)",
			profile,
			SecretAPIKey,
			SecretAccessKey,
			SecretSecretKey,
		)
	}
	return c.ark, nil
}

// requireArkAPIKey rejects AK/SK profiles for services the SDK gates to API
// key authentication (images, content-generation tasks).
func (c *clients) requireArkAPIKey(profile, service string) error {
	if c.arkAuth == arkAuthAKSK {
		return fmt.Errorf(
			"bytedance profile %q uses AK/SK, which %s does not support; use an %q profile",
			profile,
			service,
			SecretAPIKey,
		)
	}
	return nil
}

// requireSpeech returns the Doubao speech client or a profile-scoped error.
func (c *clients) requireSpeech(profile string) (*doubaospeech.Client, error) {
	if c.speech == nil {
		return nil, fmt.Errorf(
			"bytedance profile %q needs a speech credential and app_id "+
				"for Doubao speech services",
			profile,
		)
	}
	return c.speech, nil
}
