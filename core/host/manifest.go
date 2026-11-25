package host

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// The manifest is assembled here, once per authority.
//
// Not per turn, because it is a fixed fact for a session and hashing a tool schema per turn
// would be paying for the same answer again; and not in `deploy`, because two of its inputs
// only exist after the process is built: what the providers say they serve, and what the tool
// table exposes once the authority and the decorators have been applied. Values the authority
// determines (the models and tools), the deployment decides (the driver kind) and the host owns
// (the fingerprint, the build identity) are combined with those, so the manifest describes what
// actually ran rather than the bytes that described it.

// manifestHash renders one manifest component as a stable identity.
//
// The input is a decoded value, not the document's bytes: reformatting a field or adding
// a comment must not change which runs are comparable. Slices are sorted by their caller
// before they arrive, and map keys are ordered by the encoder, so two processes that built
// the same configuration hash the same bytes.
func manifestHash(value any) string {
	body, err := json.Marshal(value)
	if err != nil {
		// Every component is plain data; an empty digest is the safe direction, and it
		// shows up as "this deployment recorded no identity", not as a shared one.
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// manifestModels is the model catalog this deployment may call: the grant's list, with each
// entry expanded to what the provider declared about it.
//
// The grant's refs alone would miss the interesting part — a weight, a context limit or a
// capability change on the same model id — and the catalog alone would describe another
// agent's deployment in the same process. The intersection is the honest answer, and a
// granted model the catalog does not declare stays in as its bare ref so a typo in a grant
// cannot vanish from the manifest.
func manifestModels(provider *inference.Provider, granted []policy.ModelGrant) string {
	if len(granted) == 0 {
		return ""
	}
	entries := make([]manifestModel, 0, len(granted))
	for _, grant := range granted {
		entry := manifestModel{Ref: grant.ModelRef, Modes: grant.Modes}
		if provider != nil && provider.Catalog != nil {
			if descriptor, found := provider.Catalog.Describe(grant.ID); found {
				entry.Declared = &descriptor
			}
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i].Ref, entries[j].Ref
		if left.ID.Provider != right.ID.Provider {
			return left.ID.Provider < right.ID.Provider
		}
		if left.ID.Name != right.ID.Name {
			return left.ID.Name < right.ID.Name
		}
		return left.Profile < right.Profile
	})
	return manifestHash(entries)
}

// manifestModel is one model as the manifest records it: what the grant names, plus what
// the provider declared about it.
type manifestModel struct {
	Ref model.ModelRef
	// Modes is what the authority narrowed the model to, when it narrowed it. It is part
	// of the combination: two sessions that may call one model for different modes do not
	// hold the same authority.
	Modes    []model.Mode           `json:",omitempty"`
	Declared *model.ModelDescriptor `json:",omitempty"`
}

// manifestTools is the schema this deployment configures: the grant's exposure, after the
// process's definition decorators.
//
// It is read from a view — a fresh one, since a session's own accumulates discovery and
// budget — and not from the registry, because that view is the surface the model is offered: a
// deployment that hides a tool, or rewrites a description, is a different version combination
// even though the registry is shared. What it deliberately does not include is per-round
// trimming; a manifest that changed with one round's budget would stop naming a deployment.
func manifestTools(view tool.View) string {
	if view == nil {
		return ""
	}
	definitions := view.Definitions()
	if len(definitions) == 0 {
		return ""
	}
	sorted := make([]toolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		sorted = append(sorted, toolDefinition{
			Name:        definition.Name,
			Description: definition.Description,
			InputSchema: definition.InputSchema,
		})
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return manifestHash(sorted)
}

// toolDefinition is the part of a tool definition the manifest records.
//
// Name, description and schema are what the model reads; anything else a definition carries
// (a display label, a source hint) does not change the answer to "which schema produced this
// run", so leaving it out keeps two deployments that differ only in presentation comparable.
type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// manifestFor is manifestOf with the per-authority cache: two sessions opened with the same
// grant run the same version combination, and hashing a tool schema per open would be paying for
// the same answer twice. It is keyed by the grant's digest, which covers every field the
// manifest reads.
func (a *Agents) manifestFor(entry *deployment, grant policy.Grant, view tool.View) agent.Manifest {
	key := grant.Digest()
	entry.cacheMu.Lock()
	cached, ok := entry.manifests[key]
	entry.cacheMu.Unlock()
	if ok {
		return cached
	}
	manifest := a.manifestOf(entry.kind, grant.Models, view)
	entry.cacheMu.Lock()
	entry.manifests[key] = manifest
	entry.cacheMu.Unlock()
	return manifest
}

// manifestOf assembles one deployment's version combination from the process it runs in.
//
// Inputs come from three owners: the deployment (the driver kind), the session's authority (which
// models and tools it holds), the process (what the providers declare and what the tool table
// exposes), and the host (the deployment fingerprint and the build identity).
func (a *Agents) manifestOf(driver string, granted []policy.ModelGrant, view tool.View) agent.Manifest {
	return agent.Manifest{
		Schema:     agent.ManifestSchema,
		Deployment: a.fingerprint,
		Driver:     driver,
		Models:     manifestModels(a.res.models, granted),
		Tools:      manifestTools(view),
		Harness:    a.harness,
	}
}
