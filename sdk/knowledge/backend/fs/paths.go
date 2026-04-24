// Package fs implements knowledge.DocumentRepo, knowledge.ChunkRepo and
// knowledge.LayerRepo on top of any workspace.Workspace.
//
// Layout under <prefix>/<dataset>/:
//
//	<doc>                  raw source content (unchanged)
//	<doc>.meta.json        SourceDocument.Version + Metadata sidecar
//	.chunks.json           DerivedChunk[] for the dataset (atomic write)
//	<doc>.abstract         L0 layer text   (Q7=A: human-readable)
//	<doc>.abstract.vec     L0 layer embedding (binary, see vec.go)
//	<doc>.overview         L1 layer text
//	<doc>.overview.vec     L1 layer embedding
//	<doc>.layers.json      per-doc layer DerivedSig sidecar
//	.abstract.md           dataset-level L0 text
//	.abstract.md.vec       dataset-level L0 embedding
//	.overview.md           dataset-level L1 text
//	.overview.md.vec       dataset-level L1 embedding
//	.dataset_layers.json   dataset-level layer DerivedSig sidecar
//
// Atomic writes go through workspace.Rename(tmp, final); the workspace
// contract requires Rename to be POSIX-atomic when the medium supports
// it, so partial writes are never observable.
package fs

import (
	"path/filepath"
	"strings"
)

// DefaultPrefix is the workspace sub-tree owned by the knowledge backend.
const DefaultPrefix = "knowledge"

// chunkIndexFile is the dataset-level inverted index sidecar name.
const chunkIndexFile = ".chunks.json"

// datasetAbstractFile is the dataset-level L0 file name.
const datasetAbstractFile = ".abstract.md"

// datasetOverviewFile is the dataset-level L1 file name.
const datasetOverviewFile = ".overview.md"

// datasetLayersFile is the dataset-level layer DerivedSig sidecar.
const datasetLayersFile = ".dataset_layers.json"

// supportedDocExtensions are the file extensions FSDocumentRepo treats
// as documents when listing a dataset.
var supportedDocExtensions = []string{".md", ".markdown", ".txt"}

// docExtensions for backwards-compatible discovery; isDocument returns
// true when the file is one of those.
func isDocument(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range supportedDocExtensions {
		if ext == e {
			return true
		}
	}
	return false
}

// pathBuilder centralises path construction so layout changes touch one place.
type pathBuilder struct{ prefix string }

func newPathBuilder(prefix string) pathBuilder {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	return pathBuilder{prefix: prefix}
}

// rootDir is the top-level knowledge directory.
func (p pathBuilder) rootDir() string { return p.prefix }

// datasetDir is <prefix>/<dataset>.
func (p pathBuilder) datasetDir(datasetID string) string {
	return filepath.Join(p.prefix, datasetID)
}

// documentPath is <prefix>/<dataset>/<name>, appending ".md" when name
// has no recognised extension. This preserves legacy FSStore behaviour
// so existing on-disk knowledge bases stay readable.
func (p pathBuilder) documentPath(datasetID, name string) string {
	if !isDocument(name) {
		name += ".md"
	}
	return filepath.Join(p.prefix, datasetID, name)
}

// metaPath is <prefix>/<dataset>/<base>.meta.json (base strips the doc
// extension). Stored alongside the raw document.
func (p pathBuilder) metaPath(datasetID, name string) string {
	return filepath.Join(p.prefix, datasetID, baseName(name)+".meta.json")
}

// chunksPath is the dataset-level chunk sidecar.
func (p pathBuilder) chunksPath(datasetID string) string {
	return filepath.Join(p.prefix, datasetID, chunkIndexFile)
}

// layerPath is the per-document layer text path.
//
//	LayerAbstract -> <base>.abstract
//	LayerOverview -> <base>.overview
//	LayerDetail   -> "" (detail text lives in chunks, not in a layer file)
func (p pathBuilder) layerPath(datasetID, name, layer string) string {
	switch layer {
	case "L0":
		return filepath.Join(p.prefix, datasetID, baseName(name)+".abstract")
	case "L1":
		return filepath.Join(p.prefix, datasetID, baseName(name)+".overview")
	}
	return ""
}

// docLayersPath is the per-document DerivedSig sidecar.
func (p pathBuilder) docLayersPath(datasetID, name string) string {
	return filepath.Join(p.prefix, datasetID, baseName(name)+".layers.json")
}

// layerVecPath is the per-document layer embedding sidecar. Co-located
// with the text file with a ".vec" suffix so the two stay in lockstep.
func (p pathBuilder) layerVecPath(datasetID, name, layer string) string {
	text := p.layerPath(datasetID, name, layer)
	if text == "" {
		return ""
	}
	return text + vecSuffix
}

// datasetLayerPath is the dataset-level layer text path.
func (p pathBuilder) datasetLayerPath(datasetID, layer string) string {
	switch layer {
	case "L0":
		return filepath.Join(p.prefix, datasetID, datasetAbstractFile)
	case "L1":
		return filepath.Join(p.prefix, datasetID, datasetOverviewFile)
	}
	return ""
}

// datasetLayerVecPath is the dataset-level layer embedding sidecar.
func (p pathBuilder) datasetLayerVecPath(datasetID, layer string) string {
	text := p.datasetLayerPath(datasetID, layer)
	if text == "" {
		return ""
	}
	return text + vecSuffix
}

// datasetLayersSigPath is the dataset-level DerivedSig sidecar.
func (p pathBuilder) datasetLayersSigPath(datasetID string) string {
	return filepath.Join(p.prefix, datasetID, datasetLayersFile)
}

// baseName strips the file extension from name.
func baseName(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// isPrefixSelfEntry returns true when name equals the basename of the
// prefix directory itself. Some Workspace implementations (notably
// MemWorkspace) emit the queried directory as one of its own entries;
// callers that enumerate dataset IDs use this helper to skip it.
func (p pathBuilder) isPrefixSelfEntry(name string) bool {
	return name == filepath.Base(p.prefix)
}
