package knowledge

import "time"

// ContextLayer is the pre-v0.3.0 name for [Layer]. Retained as a type
// alias so external code that referenced knowledge.ContextLayer keeps
// compiling. The constant set (LayerAbstract / LayerOverview /
// LayerDetail) lives in types.go.
type ContextLayer = Layer

// IsValidLayer reports whether l is a recognised layer.
//
// (Method set on a type alias must live on the underlying type; provided
// as a free function to avoid colliding with future ContextLayer methods.)
func IsValidLayer(l Layer) bool {
	switch l {
	case LayerAbstract, LayerOverview, LayerDetail:
		return true
	}
	return false
}

// SourceDocument is the canonical, lossless representation of user input.
//
// It is the single source of truth for a document; every DerivedChunk and
// DerivedLayer carries a DerivedSig that points back to a particular
// SourceDocument.Version, so derived data can be detected as stale and
// recomputed deterministically.
type SourceDocument struct {
	DatasetID string
	Name      string
	Content   string
	Metadata  map[string]string

	// Version is monotonically incremented on every successful Put.
	// Derived data uses it as a freshness key.
	Version   uint64
	UpdatedAt time.Time
}

// DerivedSig binds a derived artifact to the source revision and to the
// configuration that produced it. Required on every derived object.
//
//   - SourceVer  is the SourceDocument.Version that produced this artifact.
//   - ChunkerSig is non-empty for chunk artifacts and empty for layers.
//   - PromptSig  is non-empty for layer artifacts and empty for chunks.
//   - EmbedSig   identifies the embedder, "" when no vector is attached.
type DerivedSig struct {
	SourceVer  uint64
	ChunkerSig string
	PromptSig  string
	EmbedSig   string
}

// IsStale returns true when sig was produced for an earlier source version
// or with a different chunker / prompt / embed configuration than want.
//
// EmbedSig is compared strictly, including the empty string. This makes
// vectors stale when an embedder is removed so a rebuild can clear them.
func (sig DerivedSig) IsStale(want DerivedSig) bool {
	if sig.SourceVer != want.SourceVer {
		return true
	}
	if want.ChunkerSig != "" && sig.ChunkerSig != want.ChunkerSig {
		return true
	}
	if want.PromptSig != "" && sig.PromptSig != want.PromptSig {
		return true
	}
	if sig.EmbedSig != want.EmbedSig {
		return true
	}
	return false
}

// DerivedChunk is one retrieval unit derived from a SourceDocument.
type DerivedChunk struct {
	DatasetID string
	DocName   string
	Index     int
	Offset    int
	Content   string
	Vector    []float32
	Sig       DerivedSig
}

// DerivedLayer is an LLM-produced summary of a document or dataset.
// DocName == "" denotes a dataset-level layer.
type DerivedLayer struct {
	DatasetID string
	DocName   string
	Layer     Layer
	Content   string
	Vector    []float32
	Sig       DerivedSig
}
