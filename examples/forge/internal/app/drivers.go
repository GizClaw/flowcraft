package app

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/inference/anthropic"
	"github.com/GizClaw/flowcraft/inference/openai"
)

// providers registers the protocol families this binary carries: a deployment names one of these in
// `inference.providers[].impl`, and a name that is not here fails the load with the list of what is.
//
// The table lives in the application because the platform imports no provider. Every provider's
// `Settings` is a Go value without tags, so the adapter from a written block to that value is this
// function — the alternative would freeze one provider's settings shape as the document's.
//
// Nothing per-call is decided here. What one request asks for — sampling, retention, a vendor's own
// knob — is the model node's `request.extensions` in the graph document, which is where a turn's
// decision belongs; Settings is what the deployment decides once.
func providers() func(*deploy.Impls) *deploy.Impls {
	return func(impls *deploy.Impls) *deploy.Impls {
		return impls.
			Provider(deploy.Impl(openai.ProviderID, openai.Build)).
			Provider(deploy.Impl(anthropic.ProviderID, anthropic.Build))
	}
}

// decodeSettings decodes one settings block strictly: a misspelled field is a
// build error, not a silently defaulted setting. An empty block is zero settings.
func decodeSettings(raw json.RawMessage, into any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	return nil
}
