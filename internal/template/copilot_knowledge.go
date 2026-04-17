package template

import "embed"

//go:embed copilot_reference/*.md
var copilotReferenceFS embed.FS

// CoPilotReferenceDoc describes a single embedded reference document
// for the copilot-reference Knowledge Dataset.
type CoPilotReferenceDoc struct {
	Name    string // document name (e.g. "node-types")
	Content string // full markdown content
}

// CoPilotReferenceDocs returns all embedded CoPilot reference documents.
// These are intended to be written into the `copilot-reference` Dataset
// during `ensureCoPilotApp` initialization (M6).
func CoPilotReferenceDocs() []CoPilotReferenceDoc {
	entries := []struct {
		name string
		file string
	}{
		{"topology-patterns", "copilot_reference/topology-patterns.md"},
		{"common-pitfalls", "copilot_reference/common-pitfalls.md"},
		{"memory-tools", "copilot_reference/memory-tools.md"},
	}

	docs := make([]CoPilotReferenceDoc, 0, len(entries))
	for _, e := range entries {
		data, err := copilotReferenceFS.ReadFile(e.file)
		if err != nil {
			continue
		}
		docs = append(docs, CoPilotReferenceDoc{Name: e.name, Content: string(data)})
	}
	return docs
}
