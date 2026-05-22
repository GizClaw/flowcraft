// Package factory is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/knowledge/factory instead.
// This compatibility package will be removed in v0.5.0.
package factory

import target "github.com/GizClaw/flowcraft/memory/knowledge/factory"

type (
	LocalOption     = target.LocalOption
	RetrievalOption = target.RetrievalOption
)

var (
	NewLocal              = target.NewLocal
	NewRetrieval          = target.NewRetrieval
	WithLocalChunker      = target.WithLocalChunker
	WithLocalEmbedder     = target.WithLocalEmbedder
	WithLocalPrefix       = target.WithLocalPrefix
	WithLocalTokenizer    = target.WithLocalTokenizer
	WithRetrievalChunker  = target.WithRetrievalChunker
	WithRetrievalEmbedder = target.WithRetrievalEmbedder
)
