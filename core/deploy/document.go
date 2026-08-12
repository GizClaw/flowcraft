package deploy

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Document is the top-level deployment DTO: a versioned resource area
// (the DAG) plus an agent area binding resources.
type Document struct {
	Version   string                      `json:"version,omitempty"`
	Resources resource.Resources          `json:"resources,omitempty"`
	Agents    map[string]agent.Definition `json:"agents,omitempty"`
	// Runtime is an application-runtime-owned configuration subtree.
	// Parse preserves it verbatim so the runtime layer can decode it
	// strictly without weakening strictness for the deployment schema.
	Runtime *resource.Opaque `json:"runtime,omitempty"`
}

// Validate checks the document without resolving any factory.
func (d Document) Validate() error {
	if d.Version == "" {
		return errdefs.Validationf("deployment document: version is required")
	}
	if err := d.Resources.Validate(); err != nil {
		return err
	}
	for name, definition := range d.Agents {
		if strings.TrimSpace(name) == "" {
			return errdefs.Validationf("deployment document: agent name is empty")
		}
		if err := definition.Validate(); err != nil {
			return errdefs.Validationf("deployment document: agents[%q]: %v", name, err)
		}
	}
	return nil
}
