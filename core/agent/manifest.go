package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Manifest is the version combination a run executed under.
//
// It answers "which deployment, engine, model catalog, tool schema and prompt
// bundle produced this trace" without joining every configuration store. The
// values are opaque hashes: two runs under the same manifest are comparable,
// and the per-call record still names the model that actually answered.
type Manifest struct {
	// Deployment is the checkpoint fingerprint: runtime + engine + protocol.
	Deployment Fingerprint `json:"deployment,omitempty"`
	// Engine is the engine kind the deployment selected ("graph", "a2a").
	Engine string `json:"engine,omitempty"`
	// Protocol is the exposure kind, empty when the agent is not exposed.
	Protocol string `json:"protocol,omitempty"`
	// Models is the model catalog the deployment declared.
	Models string `json:"models,omitempty"`
	// Tools is the tool schema the deployment wired.
	Tools string `json:"tools,omitempty"`
	// Prompt is the prompt bundle the engine compiled, when it has one.
	Prompt string `json:"prompt,omitempty"`
	// Harness is the host-supplied build identity.
	Harness string `json:"harness,omitempty"`
}

// ID is the manifest's identity: a stable hash of its fields.
func (m Manifest) ID() string {
	if m == (Manifest{}) {
		return ""
	}
	body, err := json.Marshal(m)
	if err != nil {
		// Every field is plain data; a marshal failure cannot happen, and an
		// empty id is the safe direction (a run without a manifest).
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
