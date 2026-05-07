package v1alpha1

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ToolPack is the wire form of a reusable bundle of tools an Agent
// can pull in. The Catalog ref names a registered ToolPackFactory
// (e.g. "recall-builtin", "kanban-builtin"); the factory returns
// a list of model.Tool implementations to be added to the
// daemon-shared tool registry under unique names.
//
// One ToolPack instance is reused across every Agent that lists
// it under engine.config.toolPacks (engine config decides whether
// to honour that key). The kanban subsystem's tools are not
// authored as a ToolPack — they are auto-injected per Dispatcher
// by the vessel runtime — but a future "shared kanban tools"
// pack would slot in cleanly under this same kind.
type ToolPack struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ToolPackSpec `json:"spec" yaml:"spec"`
}

type ToolPackSpec struct {
	Ref    string         `json:"ref" yaml:"ref"`
	Config map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

func (t ToolPack) GetTypeMeta() TypeMeta     { return t.TypeMeta }
func (t ToolPack) GetObjectMeta() ObjectMeta { return t.ObjectMeta }

func (t ToolPack) Validate() error {
	if err := t.ObjectMeta.Validate(KindToolPack); err != nil {
		return err
	}
	if t.TypeMeta.APIVersion != APIVersion {
		return errdefs.Validationf("vesseld ToolPack %q: apiVersion %q != %q", t.Name, t.TypeMeta.APIVersion, APIVersion)
	}
	if t.TypeMeta.Kind != KindToolPack {
		return errdefs.Validationf("vesseld ToolPack %q: kind %q != %q", t.Name, t.TypeMeta.Kind, KindToolPack)
	}
	if t.Spec.Ref == "" {
		return errdefs.Validationf("vesseld ToolPack %q: spec.ref is required", t.Name)
	}
	return nil
}
