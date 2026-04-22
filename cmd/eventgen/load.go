package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type manifestFile struct {
	SpecVersion int                    `yaml:"spec_version"`
	GeneratedAt string                 `yaml:"generated_at"`
	Partitions  []PartitionDef         `yaml:"partitions"`
	Categories  map[string]CategoryDef `yaml:"categories"`
	Lint        LintConfig             `yaml:"lint"`
	Includes    []string               `yaml:"includes"`
}

func loadSpec(manifestPath string) (*Spec, error) {
	root := filepath.Dir(manifestPath)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var mf manifestFile
	if err := yaml.Unmarshal(data, &mf); err != nil {
		return nil, err
	}
	if len(mf.Includes) == 0 {
		return nil, fmt.Errorf("missing includes in manifest")
	}
	spec := &Spec{
		SpecVersion: mf.SpecVersion,
		Partitions:  mf.Partitions,
		Categories:  mf.Categories,
		Lint:        mf.Lint,
		Payloads:    make(map[string]PayloadDef),
	}
	for _, rel := range mf.Includes {
		p := filepath.Join(root, rel)
		if err := loadDomainFile(p, spec); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
	}
	for i := range spec.Events {
		pt, err := resolvePayloadType(root, spec.Events[i].PayloadRef, spec)
		if err != nil {
			return nil, fmt.Errorf("event %s: %w", spec.Events[i].Name, err)
		}
		spec.Events[i].PayloadType = pt
	}
	slices.SortFunc(spec.Events, func(a, b EventDef) int { return strings.Compare(a.Name, b.Name) })
	return spec, nil
}

func loadDomainFile(path string, spec *Spec) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Unmarshal as map[string]any first: a single struct with both `events` and
	// `payloads` maps confuses go-yaml when nested maps look like FieldDef maps.
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}
	domain, _ := raw["domain"].(string)
	if domain == "" {
		return fmt.Errorf("missing domain in %s", path)
	}
	evMap, _ := raw["events"].(map[string]any)
	for name, v := range evMap {
		eb, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("event %s: %w", name, err)
		}
		var ev EventDef
		if err := yaml.Unmarshal(eb, &ev); err != nil {
			return fmt.Errorf("event %s: %w", name, err)
		}
		ev.Name = name
		ev.Domain = domain
		if !strings.HasPrefix(ev.Name, ev.Domain+".") {
			return fmt.Errorf("event %q must start with domain %q.", ev.Name, ev.Domain+".")
		}
		spec.Events = append(spec.Events, ev)
	}
	pl, _ := raw["payloads"].(map[string]any)
	for pname, v := range pl {
		pb, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("payload %s: %w", pname, err)
		}
		var fields map[string]FieldDef
		if err := yaml.Unmarshal(pb, &fields); err != nil {
			return fmt.Errorf("payload %s: %w", pname, err)
		}
		spec.Payloads[pname] = PayloadDef{Name: pname, Fields: fields}
	}
	return nil
}

func resolvePayloadType(root, ref string, spec *Spec) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("empty payload_ref")
	}
	if strings.Contains(ref, "#") {
		filePart, name, ok := strings.Cut(ref, "#")
		if !ok || name == "" {
			return "", fmt.Errorf("invalid external payload ref %q", ref)
		}
		p := filepath.Join(root, filepath.FromSlash(strings.TrimSpace(filePart)))
		if err := ensurePayloadFromFile(p, name, spec); err != nil {
			return "", err
		}
		return name, nil
	}
	if _, ok := spec.Payloads[ref]; !ok {
		return "", fmt.Errorf("unknown payload %q", ref)
	}
	return ref, nil
}

func ensurePayloadFromFile(path, defName string, spec *Spec) error {
	if _, ok := spec.Payloads[defName]; ok {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc struct {
		Definitions map[string]map[string]FieldDef `yaml:"definitions"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	fields, ok := doc.Definitions[defName]
	if !ok {
		return fmt.Errorf("%s: no definitions.%s", path, defName)
	}
	spec.Payloads[defName] = PayloadDef{Name: defName, Fields: fields}
	return nil
}
