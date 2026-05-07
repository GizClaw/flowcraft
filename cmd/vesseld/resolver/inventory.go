package resolver

import (
	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Inventory is the bag of typed apispec documents the loader
// emitted, indexed by metadata.name within each kind for fast
// reference resolution. Built once at the start of Resolve and
// passed to every per-kind resolution helper.
//
// We keep maps rather than slices because every cross-document
// reference (Vessel→Agent, Vessel→Probe, Agent→LLMProfile, ...)
// is a name lookup; slices would force O(N) walks per lookup and
// quickly become the resolver's bottleneck on large configs.
type Inventory struct {
	Daemons       []v1alpha1.Daemon
	Vessels       []v1alpha1.Vessel
	Agents        map[string]v1alpha1.Agent
	LLMProfiles   map[string]v1alpha1.LLMProfile
	Probes        map[string]v1alpha1.Probe
	ToolPacks     map[string]v1alpha1.ToolPack
	HistoryStores map[string]v1alpha1.HistoryStore
	Secrets       []v1alpha1.Secret
}

// buildInventory partitions the loader output into the per-kind
// indices, enforcing the "exactly one Daemon" rule and the
// "no duplicate apiVersion+kind+name" rule the loader does not
// itself check across documents (the loader dedupes inside one
// file; the resolver dedupes across the entire input).
//
// Returns an Errors aggregate so the user sees every duplicate /
// missing-Daemon issue in one pass instead of one per re-run.
func buildInventory(objs []apispec.Object) (Inventory, *Errors) {
	inv := Inventory{
		Agents:        map[string]v1alpha1.Agent{},
		LLMProfiles:   map[string]v1alpha1.LLMProfile{},
		Probes:        map[string]v1alpha1.Probe{},
		ToolPacks:     map[string]v1alpha1.ToolPack{},
		HistoryStores: map[string]v1alpha1.HistoryStore{},
	}
	errs := &Errors{}
	seen := map[string]struct{}{} // "kind/name" → {}

	dedupe := func(kind, name string) bool {
		key := kind + "/" + name
		if _, ok := seen[key]; ok {
			errs.add(errdefs.Validationf("vesseld: duplicate %s %q", kind, name))
			return false
		}
		seen[key] = struct{}{}
		return true
	}

	for _, obj := range objs {
		if err := obj.Validate(); err != nil {
			errs.add(err)
			continue
		}
		switch o := obj.(type) {
		case v1alpha1.Daemon:
			if dedupe(v1alpha1.KindDaemon, o.Name) {
				inv.Daemons = append(inv.Daemons, o)
			}
		case v1alpha1.Vessel:
			if dedupe(v1alpha1.KindVessel, o.Name) {
				inv.Vessels = append(inv.Vessels, o)
			}
		case v1alpha1.Agent:
			if dedupe(v1alpha1.KindAgent, o.Name) {
				inv.Agents[o.Name] = o
			}
		case v1alpha1.LLMProfile:
			if dedupe(v1alpha1.KindLLMProfile, o.Name) {
				inv.LLMProfiles[o.Name] = o
			}
		case v1alpha1.Probe:
			if dedupe(v1alpha1.KindProbe, o.Name) {
				inv.Probes[o.Name] = o
			}
		case v1alpha1.ToolPack:
			if dedupe(v1alpha1.KindToolPack, o.Name) {
				inv.ToolPacks[o.Name] = o
			}
		case v1alpha1.HistoryStore:
			if dedupe(v1alpha1.KindHistoryStore, o.Name) {
				inv.HistoryStores[o.Name] = o
			}
		case v1alpha1.Secret:
			if dedupe(v1alpha1.KindSecret, o.Name) {
				inv.Secrets = append(inv.Secrets, o)
			}
		default:
			errs.add(errdefs.Validationf("vesseld resolver: unhandled object type %T", obj))
		}
	}
	if len(inv.Daemons) == 0 {
		errs.add(errdefs.Validationf("vesseld: configuration has no kind: Daemon document (exactly one is required)"))
	}
	if len(inv.Daemons) > 1 {
		errs.add(errdefs.Validationf("vesseld: configuration has %d kind: Daemon documents (only one is allowed)", len(inv.Daemons)))
	}
	if len(inv.Vessels) == 0 {
		errs.add(errdefs.Validationf("vesseld: configuration has no kind: Vessel documents"))
	}
	return inv, errs
}
