package deploy

import (
	"context"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/goccy/go-json"
)

// SupportedVersion is the only document version this loader reads. A document that declares
// another one is refused instead of being interpreted optimistically: the fields a newer
// version adds would be rejected as unknown anyway, and "which version is this" is the first
// thing a reader of a failed deployment asks.
const SupportedVersion = 1

// processDocument is the process document's shape.
//
// Every section is a list of named entries with an implementation and a settings block. The settings
// stay raw here: their schema belongs to the registered factory, which is the only thing that
// knows what a backend or a provider needs.
// processDocument is the process document's shape.
//
// It is grouped by concern, the way the deployment it describes is: one section for the
// boundary, one for the models, one for the tools, one for the skills. Inside a section every
// entry names an implementation and carries that implementation's settings as an opaque block,
// because the schema of a settings block belongs to the implementation — the application
// registers it, so this package cannot know it.
type processDocument struct {
	Version   int               `json:"version"`
	Sandbox   []entryDocument   `json:"sandbox,omitempty"`
	Inference inferenceDocument `json:"inference,omitempty"`
	Tools     toolsDocument     `json:"tools,omitempty"`
	Skills    skillsDocument    `json:"skills,omitempty"`
}

// inferenceDocument is the model half: the targets this process serves, and how a call fails
// over between them.
type inferenceDocument struct {
	Providers []entryDocument `json:"providers,omitempty"`
	Route     routeDocument   `json:"route,omitempty"`
}

// toolsDocument is what the model can call: the sources its tools come from, and the chain every
// call passes through.
type toolsDocument struct {
	Sources    []entryDocument `json:"sources,omitempty"`
	Middleware []entryDocument `json:"middleware,omitempty"`
}

// skillsDocument is where skill discovery looks. Roots are a process fact; the index they
// produce is a session fact, because discovery reads through the session's tree.
type skillsDocument struct {
	Roots []string `json:"roots,omitempty"`
}

// routeDocument is the model-call policy a process may state.
//
// Selectors are Go values and stay out of the document; what is written here is the retry
// half, which is entirely values.
type routeDocument struct {
	AttemptsPerTarget int    `json:"attempts_per_target,omitempty"`
	MaxTargets        int    `json:"max_targets,omitempty"`
	BaseDelay         string `json:"base_delay,omitempty"`
	MaxDelay          string `json:"max_delay,omitempty"`
}

// observabilityDocument is how much of a turn to record.
type observabilityDocument struct {
	Trace  string `json:"trace,omitempty"`
	Bodies *bool  `json:"bodies,omitempty"`
}

// getVersion implements versioned.
func (d processDocument) getVersion() int { return d.Version }

// entryDocument is one named resource with a selected implementation.
type entryDocument struct {
	Name     string          `json:"name"`
	Impl     string          `json:"impl"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// agentDocument is the agent document's shape: one file, a list of agents.
//
// A fleet is described once and then tuned per seat, and a document with a single agent is
// just a list of one — so there is no "single agent" variant to keep in step.
//
// It has no runtime section on purpose: what an agent may use is its grant, not a private copy
// of the process. The program section names an implementation registered on the process; the grant is
// policy's own type, decoded and validated by it.
type agentDocument struct {
	Version int          `json:"version"`
	Agents  []agentEntry `json:"agents"`
}

// agentEntry is one agent within a document.
type agentEntry struct {
	// Name is the agent's identity: what a host addresses it by, and what layer merging uses
	// to decide which entry an overlay is rewriting.
	Name          string                `json:"name"`
	Program       programDocument       `json:"program"`
	Grant         policy.Grant          `json:"grant,omitempty"`
	Observability observabilityDocument `json:"observability,omitempty"`
	Hooks         []hookDocument        `json:"hooks,omitempty"`
}

// hookDocument is one lifecycle policy: an implementation and its settings.
type hookDocument struct {
	Impl     string          `json:"impl"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// getVersion implements versioned.
func (d agentDocument) getVersion() int { return d.Version }

// programDocument selects what runs the conversation.
type programDocument struct {
	Impl     string          `json:"impl"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// versioned is what both documents share: the version they were written for.
type versioned interface {
	// getVersion is the document's declared version.
	getVersion() int
}

// loadDocument merges the layers and decodes the result strictly: YAML or JSON, unknown
// fields refused, one document per layer.
func loadDocument[T versioned](ctx context.Context, layers []config.Layer, label string) (T, error) {
	var zero T
	// Provenance is off: this loader discards the merged document, and a provenance it never
	// reads is a walk over every layer per load. Turning it on is a one-line change when a
	// `deploy describe` needs to say which layer wrote a field.
	doc, _, err := config.Load[T](ctx, layers, config.Strict[T],
		config.WithLabel(label), config.WithoutProvenance())
	if err != nil {
		return zero, err
	}
	if doc.getVersion() != SupportedVersion {
		return zero, errdefs.Validationf(
			"deploy: %s: document version %d, want %d", label, doc.getVersion(), SupportedVersion)
	}
	return doc, nil
}
