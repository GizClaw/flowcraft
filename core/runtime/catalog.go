package runtime

import (
	"reflect"
	"sort"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// mergedCatalog is one target's model facts: what the driver declares, plus what
// the deployment filled in for models the driver does not know.
//
// The driver wins. Its catalog ships with the driver version, while the
// deployment file is written by hand — letting the hand-written copy override the
// shipped one makes a configuration silently stale after an upgrade. A
// contradiction is therefore an assembly error, not a precedence question.
type mergedCatalog struct {
	target string
	byName map[string]model.ModelDescriptor
}

var _ inference.Catalog = (*mergedCatalog)(nil)

// mergeCatalog combines the driver's declarations with the written ones.
//
// It returns nil when neither side declares anything: no catalog means "unknown",
// and routing treats unknown as "candidate" rather than "not served".
func mergeCatalog(target string, driver inference.Catalog, declared []model.ModelDescriptor) (inference.Catalog, error) {
	byName := make(map[string]model.ModelDescriptor)
	if driver != nil {
		for _, descriptor := range driver.Models() {
			byName[descriptor.ID.Name] = restamp(descriptor, target)
		}
	}
	for _, descriptor := range declared {
		stamped := restamp(descriptor, target)
		existing, found := byName[stamped.ID.Name]
		if !found {
			byName[stamped.ID.Name] = stamped
			continue
		}
		if err := sameFacts(target, existing, stamped); err != nil {
			return nil, err
		}
	}
	if len(byName) == 0 {
		return nil, nil
	}
	return &mergedCatalog{target: target, byName: byName}, nil
}

func restamp(descriptor model.ModelDescriptor, target string) model.ModelDescriptor {
	descriptor = descriptor.Clone()
	descriptor.ID.Provider = target
	return descriptor
}

// sameFacts rejects a declaration that contradicts the driver's catalog.
func sameFacts(target string, driver, declared model.ModelDescriptor) error {
	if reflect.DeepEqual(driver.Capabilities, declared.Capabilities) &&
		reflect.DeepEqual(driver.Limits, declared.Limits) {
		return nil
	}
	// The declaration may be partial (only what the deployment knows); compare
	// only the fields it actually set.
	if !kindsCovered(declared.Capabilities.Inputs, driver.Capabilities.Inputs) ||
		!kindsCovered(declared.Capabilities.Outputs, driver.Capabilities.Outputs) {
		return errdefs.Validationf(
			"inference: model %q on target %q declares content kinds the driver catalog does not",
			declared.ID.Name, target)
	}
	if !sameLimit(declared.Limits.MaxInputTokens, driver.Limits.MaxInputTokens) ||
		!sameLimit(declared.Limits.MaxOutputTokens, driver.Limits.MaxOutputTokens) ||
		!sameLimit(declared.Limits.MaxInputBytes, driver.Limits.MaxInputBytes) {
		return errdefs.Validationf(
			"inference: model %q on target %q declares limits that differ from the driver catalog",
			declared.ID.Name, target)
	}
	return nil
}

// kindsCovered reports whether the written kinds are a subset of what the driver
// declares. An empty list on either side is "undeclared", which contradicts
// nothing: the deployment may know less than the driver, and a driver may
// declare nothing at all.
func kindsCovered(declared, driver []message.PartKind) bool {
	if len(declared) == 0 || len(driver) == 0 {
		return true
	}
	set := make(map[message.PartKind]struct{}, len(driver))
	for _, kind := range driver {
		set[kind] = struct{}{}
	}
	for _, kind := range declared {
		if _, ok := set[kind]; !ok {
			return false
		}
	}
	return true
}

func sameLimit[T comparable](declared, driver *T) bool {
	if declared == nil || driver == nil {
		return true
	}
	return *declared == *driver
}

// Models implements inference.Catalog.
func (c *mergedCatalog) Models() []model.ModelDescriptor {
	out := make([]model.ModelDescriptor, 0, len(c.byName))
	for _, descriptor := range c.byName {
		out = append(out, descriptor)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Name < out[j].ID.Name })
	return out
}

// Describe implements inference.Catalog.
func (c *mergedCatalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	descriptor, found := c.byName[id.Name]
	return descriptor, found
}
