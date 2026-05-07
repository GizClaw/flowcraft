package v1alpha1

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// HistoryStore is the wire form of a shared history.History
// instance. The Catalog ref names a HistoryFactory ("buffer",
// "compacted" in v0.1.0; future entries can wrap Postgres /
// Redis-backed stores).
//
// One HistoryStore is shared across every Vessel that references
// it in spec.history; the resolver constructs the instance once
// and passes the same pointer to every Captain.
type HistoryStore struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       HistoryStoreSpec `json:"spec" yaml:"spec"`
}

type HistoryStoreSpec struct {
	Ref    string         `json:"ref" yaml:"ref"`
	Config map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

func (h HistoryStore) GetTypeMeta() TypeMeta     { return h.TypeMeta }
func (h HistoryStore) GetObjectMeta() ObjectMeta { return h.ObjectMeta }

func (h HistoryStore) Validate() error {
	if err := h.ObjectMeta.Validate(KindHistoryStore); err != nil {
		return err
	}
	if h.TypeMeta.APIVersion != APIVersion {
		return errdefs.Validationf("vesseld HistoryStore %q: apiVersion %q != %q", h.Name, h.TypeMeta.APIVersion, APIVersion)
	}
	if h.TypeMeta.Kind != KindHistoryStore {
		return errdefs.Validationf("vesseld HistoryStore %q: kind %q != %q", h.Name, h.TypeMeta.Kind, KindHistoryStore)
	}
	if h.Spec.Ref == "" {
		return errdefs.Validationf("vesseld HistoryStore %q: spec.ref is required", h.Name)
	}
	return nil
}
