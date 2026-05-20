package recall

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/lens"
	entitylens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/entity"
	evidencelens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/evidence"
	graphlens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/graph"
	profilelens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/profile"
	relationlens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/relation"
	retrievallens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/retrieval"
	timelinelens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/timeline"
)

// wireDefaultLenses registers the standard v2 lens set in planner
// source order. Graph is omitted when graphEnabled is false.
// Evidence is appended when withEvidence is true.
func wireDefaultLenses(reg *lens.Registry, graphEnabled, withEvidence bool) {
	if reg == nil {
		return
	}
	reg.Register(retrievallens.Lens{})
	reg.Register(entitylens.Lens{})
	if graphEnabled {
		reg.Register(graphlens.Lens{})
	}
	reg.Register(relationlens.Lens{})
	reg.Register(profilelens.Lens{})
	reg.Register(timelinelens.Lens{})
	if withEvidence {
		reg.Register(evidencelens.Lens{})
	}
}
