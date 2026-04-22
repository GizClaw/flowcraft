package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

func writeJSONSchemas(spec *Spec, contractsDir string) error {
	if err := os.MkdirAll(contractsDir, 0o755); err != nil {
		return err
	}
	env := map[string]any{
		"$schema":    "https://json-schema.org/draft/2020-12/schema",
		"title":      "FlowCraftEnvelope",
		"type":       "object",
		"required":   []string{"seq", "partition", "type", "version", "category", "ts", "payload"},
		"properties": envelopeProps(),
	}
	if err := writeJSON(filepath.Join(contractsDir, "envelope.schema.json"), env); err != nil {
		return err
	}
	defs := make(map[string]any)
	names := make([]string, 0, len(spec.Payloads))
	for n := range spec.Payloads {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		defs[n] = payloadJSONSchema(spec.Payloads[n])
	}
	root := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"title":                "FlowCraftPayloads",
		"type":                 "object",
		"additionalProperties": false,
		"properties":           defs,
	}
	return writeJSON(filepath.Join(contractsDir, "payloads.schema.json"), root)
}

func envelopeProps() map[string]any {
	return map[string]any{
		"seq":       map[string]any{"type": "integer"},
		"partition": map[string]any{"type": "string"},
		"type":      map[string]any{"type": "string"},
		"version":   map[string]any{"type": "integer"},
		"category":  map[string]any{"type": "string"},
		"ts":        map[string]any{"type": "string", "format": "date-time"},
		"payload":   map[string]any{"type": "object"},
		"actor": map[string]any{
			"type":     "object",
			"required": []string{"id", "kind", "realm_id"},
			"properties": map[string]any{
				"id":       map[string]any{"type": "string"},
				"kind":     map[string]any{"type": "string"},
				"realm_id": map[string]any{"type": "string"},
			},
		},
		"trace_id": map[string]any{"type": "string"},
		"span_id":  map[string]any{"type": "string"},
	}
}

func payloadJSONSchema(p PayloadDef) map[string]any {
	req := make([]string, 0)
	props := make(map[string]any)
	keys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f := p.Fields[k]
		props[k] = fieldJSONSchema(f)
		if f.Required {
			req = append(req, k)
		}
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             req,
		"properties":           props,
	}
}

func fieldJSONSchema(f FieldDef) map[string]any {
	switch f.Type {
	case "string", "timestamp":
		return map[string]any{"type": "string"}
	case "int64", "int32":
		return map[string]any{"type": "integer"}
	case "bool":
		return map[string]any{"type": "boolean"}
	case "float64":
		return map[string]any{"type": "number"}
	case "bytes":
		return map[string]any{"type": "string", "contentEncoding": "base64"}
	case "any":
		return map[string]any{}
	case "array":
		item := map[string]any{"type": "string"}
		if f.ItemType == "string" {
			item = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "array", "items": item}
	case "map":
		additional := map[string]any{}
		if f.ValueType == "string" {
			additional = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "object", "additionalProperties": additional}
	default:
		return map[string]any{}
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
