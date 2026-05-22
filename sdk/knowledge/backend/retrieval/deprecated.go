// Package retrieval is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/knowledge/backend/retrieval instead.
// This compatibility package will be removed in v0.5.0.
package retrieval

import target "github.com/GizClaw/flowcraft/memory/knowledge/backend/retrieval"

type (
	RetrievalChunkRepo = target.RetrievalChunkRepo
	RetrievalLayerRepo = target.RetrievalLayerRepo
)

var (
	NewChunkRepo = target.NewChunkRepo
	NewLayerRepo = target.NewLayerRepo
)
