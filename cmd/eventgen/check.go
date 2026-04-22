package main

import (
	"fmt"
	"os"
	"reflect"
	"sort"

	"gopkg.in/yaml.v3"
)

// snapshot is a frozen compatibility surface for `eventgen -mode=check`.
//
// It captures the public contract of every event: its versioned shape +
// payload field set. Payload-level evolution rules in docs/event-sourcing-plan.md
// §3.7 are enforced by diffing snapshots between baseline and current.
type snapshot struct {
	Events map[string]snapshotEvent `yaml:"events"`
}

type snapshotEvent struct {
	Version     int                      `yaml:"version"`
	Category    string                   `yaml:"category"`
	Partition   string                   `yaml:"partition"`
	PayloadType string                   `yaml:"payload_type"`
	Deprecated  bool                     `yaml:"deprecated,omitempty"`
	Fields      map[string]snapshotField `yaml:"fields,omitempty"`
}

type snapshotField struct {
	Type      string `yaml:"type"`
	Required  bool   `yaml:"required,omitempty"`
	ItemType  string `yaml:"item_type,omitempty"`
	ValueType string `yaml:"value_type,omitempty"`
}

func loadSnapshot(path string) (*snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s snapshot
	if err := yaml.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func buildSnapshot(spec *Spec) *snapshot {
	s := &snapshot{Events: make(map[string]snapshotEvent)}
	for _, ev := range spec.Events {
		se := snapshotEvent{
			Version:     ev.Version,
			Category:    ev.Category,
			Partition:   ev.Partition,
			PayloadType: ev.PayloadType,
			Deprecated:  ev.Deprecated,
			Fields:      map[string]snapshotField{},
		}
		if pl, ok := spec.Payloads[ev.PayloadType]; ok {
			for fname, fd := range pl.Fields {
				se.Fields[fname] = snapshotField{
					Type:      fd.Type,
					Required:  fd.Required,
					ItemType:  fd.ItemType,
					ValueType: fd.ValueType,
				}
			}
		}
		s.Events[ev.Name] = se
	}
	return s
}

// checkEvolution enforces docs §3.7 compatibility matrix.
//
// Rules summary (denoting B = baseline, C = current):
//
//   - Removed event: fail unless B.deprecated == true (then permitted to disappear).
//   - C.version < B.version: fail (version regression).
//   - Same version (C.version == B.version):
//   - category / partition / payload_type changed: fail (need version bump).
//   - any baseline field removed: fail.
//   - new field present in C and absent from B and required=true: fail.
//   - field type / item_type / value_type changed: fail.
//   - field flipped optional -> required: fail.
//
// Field renames cannot be detected without explicit metadata; they manifest as
// drop + add and are caught by the "removed field" rule above.
func checkEvolution(current *Spec, baselinePath string) []error {
	base, err := loadSnapshot(baselinePath)
	if err != nil {
		return []error{fmt.Errorf("load baseline: %w", err)}
	}
	cur := buildSnapshot(current)
	var errs []error

	// Stable iteration for deterministic error ordering.
	baseNames := make([]string, 0, len(base.Events))
	for n := range base.Events {
		baseNames = append(baseNames, n)
	}
	sort.Strings(baseNames)

	for _, name := range baseNames {
		be := base.Events[name]
		ce, ok := cur.Events[name]
		if !ok {
			if be.Deprecated {
				continue
			}
			errs = append(errs, fmt.Errorf("event %q removed without prior deprecated:true (mark deprecated for >=1 release first)", name))
			continue
		}
		if ce.Version < be.Version {
			errs = append(errs, fmt.Errorf("event %q: version regressed %d -> %d", name, be.Version, ce.Version))
			continue
		}
		if ce.Version > be.Version {
			// Major bump: anything goes, baseline shape no longer applies.
			continue
		}
		// Same version: structural compatibility required.
		if ce.Category != be.Category {
			errs = append(errs, fmt.Errorf("event %q: category changed %q -> %q at same version (bump version)", name, be.Category, ce.Category))
		}
		if ce.Partition != be.Partition {
			errs = append(errs, fmt.Errorf("event %q: partition changed %q -> %q at same version (bump version)", name, be.Partition, ce.Partition))
		}
		if ce.PayloadType != be.PayloadType {
			errs = append(errs, fmt.Errorf("event %q: payload_type changed %q -> %q at same version (bump version)", name, be.PayloadType, ce.PayloadType))
		}
		// Field-level diff.
		fieldNames := make([]string, 0, len(be.Fields))
		for f := range be.Fields {
			fieldNames = append(fieldNames, f)
		}
		sort.Strings(fieldNames)
		for _, f := range fieldNames {
			bf := be.Fields[f]
			cf, present := ce.Fields[f]
			if !present {
				errs = append(errs, fmt.Errorf("event %q: payload field %q removed at same version (bump version or restore field)", name, f))
				continue
			}
			if cf.Type != bf.Type {
				errs = append(errs, fmt.Errorf("event %q: payload field %q type changed %q -> %q at same version", name, f, bf.Type, cf.Type))
			}
			if cf.ItemType != bf.ItemType {
				errs = append(errs, fmt.Errorf("event %q: payload field %q item_type changed %q -> %q at same version", name, f, bf.ItemType, cf.ItemType))
			}
			if cf.ValueType != bf.ValueType {
				errs = append(errs, fmt.Errorf("event %q: payload field %q value_type changed %q -> %q at same version", name, f, bf.ValueType, cf.ValueType))
			}
			if !bf.Required && cf.Required {
				errs = append(errs, fmt.Errorf("event %q: payload field %q tightened optional -> required at same version", name, f))
			}
			// required -> optional is allowed (loosening).
		}
		// New fields: only optional ones allowed at same version.
		curFields := make([]string, 0, len(ce.Fields))
		for f := range ce.Fields {
			curFields = append(curFields, f)
		}
		sort.Strings(curFields)
		for _, f := range curFields {
			if _, ok := be.Fields[f]; ok {
				continue
			}
			cf := ce.Fields[f]
			if cf.Required {
				errs = append(errs, fmt.Errorf("event %q: new payload field %q is required at same version (must be optional or bump version)", name, f))
			}
		}
	}
	return errs
}

func writeSnapshotFile(path string, spec *Spec) error {
	snap := buildSnapshot(spec)
	b, err := yaml.Marshal(snap)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func diffSnapshots(cur, base *snapshot) string {
	var names []string
	for n := range cur.Events {
		names = append(names, n)
	}
	sort.Strings(names)
	var b string
	for _, n := range names {
		c := cur.Events[n]
		p, ok := base.Events[n]
		if !ok {
			b += fmt.Sprintf("+ %s (new, version=%d)\n", n, c.Version)
			continue
		}
		if !reflect.DeepEqual(c, p) {
			b += fmt.Sprintf("~ %s\n", n)
			if c.Version != p.Version {
				b += fmt.Sprintf("    version: %d -> %d\n", p.Version, c.Version)
			}
			if c.Category != p.Category {
				b += fmt.Sprintf("    category: %s -> %s\n", p.Category, c.Category)
			}
			if c.Partition != p.Partition {
				b += fmt.Sprintf("    partition: %s -> %s\n", p.Partition, c.Partition)
			}
			if c.PayloadType != p.PayloadType {
				b += fmt.Sprintf("    payload_type: %s -> %s\n", p.PayloadType, c.PayloadType)
			}
			b += diffFields(p.Fields, c.Fields)
		}
	}
	for n := range base.Events {
		if _, ok := cur.Events[n]; !ok {
			b += fmt.Sprintf("- %s (removed)\n", n)
		}
	}
	if b == "" {
		return "(no differences)\n"
	}
	return b
}

func diffFields(base, cur map[string]snapshotField) string {
	all := map[string]struct{}{}
	for k := range base {
		all[k] = struct{}{}
	}
	for k := range cur {
		all[k] = struct{}{}
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out string
	for _, k := range keys {
		bf, hasB := base[k]
		cf, hasC := cur[k]
		switch {
		case !hasB && hasC:
			out += fmt.Sprintf("    + field %s (%s, required=%v)\n", k, cf.Type, cf.Required)
		case hasB && !hasC:
			out += fmt.Sprintf("    - field %s\n", k)
		case bf != cf:
			out += fmt.Sprintf("    ~ field %s: %+v -> %+v\n", k, bf, cf)
		}
	}
	return out
}
