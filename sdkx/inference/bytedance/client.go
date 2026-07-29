package bytedance

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdkx/inference/config"

	doubaospeech "github.com/GizClaw/doubao-speech-go"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
)

// profileMaterial is one credential profile after secret resolution: the
// decoded profile Spec plus the secret values this provider recognizes.
type profileMaterial struct {
	spec   ProfileSpec
	apiKey string
	speech string // speech API key override; empty means use apiKey
}

// clients bundles the service clients one profile needs. ark is nil when the
// profile has no api_key; speech is nil when no speech credential/app ID is
// available. Drivers check the client they need at open time.
type clients struct {
	ark    *arkruntime.Client
	speech *doubaospeech.Client
}

func newProfileMaterial(profile config.ResolvedProfile) (profileMaterial, error) {
	spec, err := decodeProfileSpec(profile.Spec)
	if err != nil {
		return profileMaterial{}, err
	}
	material := profileMaterial{spec: spec}
	for name := range profile.Secrets {
		switch name {
		case SecretAPIKey, SecretSpeechAPIKey:
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
	if material.apiKey == "" && material.speech == "" {
		return profileMaterial{}, fmt.Errorf(
			"bytedance profile %q needs at least one of %q / %q",
			profile.ID,
			SecretAPIKey,
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
	built := &clients{}
	if m.apiKey != "" {
		options := []arkruntime.ConfigOption{}
		if spec.BaseURL != "" {
			options = append(options, arkruntime.WithBaseUrl(spec.BaseURL))
		}
		if spec.Region != "" {
			options = append(options, arkruntime.WithRegion(spec.Region))
		}
		built.ark = arkruntime.NewClientWithApiKey(m.apiKey, options...)
	}
	speechKey := m.speech
	if speechKey == "" {
		speechKey = m.apiKey
	}
	if speechKey != "" && m.spec.AppID != "" {
		options := []doubaospeech.Option{doubaospeech.WithAPIKey(speechKey)}
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
			"bytedance profile %q has no %q for Ark services",
			profile,
			SecretAPIKey,
		)
	}
	return c.ark, nil
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
