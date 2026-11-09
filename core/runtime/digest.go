package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Digest identifies the configuration a runtime was built from.
//
// A checkpoint records it so a resume can refuse to continue a conversation
// under a different model, boundary or permission set. Continuing silently is
// the most dangerous failure this layer can have: the transcript says one thing
// about what was allowed, and the new runtime enforces another.
type Digest string

// Digest returns the canonical digest of this configuration.
//
// Two properties matter more than the hash itself:
//
//   - Stability. The digest is computed over a canonical form, so reordering a
//     map or re-reading the same file does not change it.
//   - Redaction. Settings blocks may hold resolved credentials, so only their
//     key names enter the digest. Rotating a key must not invalidate every
//     checkpoint, and a digest must never be a way to confirm a guessed secret.
func (c Config) Digest() Digest {
	shadow := c.redacted()
	body, err := json.Marshal(shadow)
	if err != nil {
		// json.Marshal of a struct built from plain data cannot fail; if it ever
		// does, an empty digest fails every resume, which is the safe direction.
		return ""
	}
	sum := sha256.Sum256(body)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// redacted returns a copy whose secret-bearing maps are reduced to key names.
func (c Config) redacted() Config {
	providers := make([]ProviderConfig, len(c.Inference.Providers))
	for i, provider := range c.Inference.Providers {
		providers[i] = provider
		providers[i].Settings = redactMap(provider.Settings)
	}
	c.Inference.Providers = providers

	servers := make([]MCPServerConfig, len(c.Tools.MCP.Servers))
	for i, server := range c.Tools.MCP.Servers {
		servers[i] = server
		servers[i].Env = redactStrings(server.Env)
		servers[i].Headers = redactStrings(server.Headers)
	}
	c.Tools.MCP.Servers = servers

	middleware := make([]MiddlewareConfig, len(c.Tools.Middleware))
	for i, entry := range c.Tools.Middleware {
		middleware[i] = entry
		middleware[i].Settings = redactMap(entry.Settings)
	}
	c.Tools.Middleware = middleware
	return c
}

// redactMap keeps the keys and drops the values: a settings map is a schema, and
// only its shape is part of what a runtime is.
func redactMap(settings json.RawMessage) json.RawMessage {
	if len(settings) == 0 || string(settings) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(settings, &fields); err != nil {
		// A settings blob that is not an object still names nothing; a digest
		// must never confirm a guessed secret.
		return json.RawMessage("{}")
	}
	for key := range fields {
		fields[key] = json.RawMessage(`"<redacted>"`)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

func redactStrings(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key := range values {
		out[key] = "<redacted>"
	}
	return out
}
