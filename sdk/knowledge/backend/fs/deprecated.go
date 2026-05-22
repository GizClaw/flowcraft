// Package fs is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/knowledge/backend/fs instead.
// This compatibility package will be removed in v0.5.0.
package fs

import target "github.com/GizClaw/flowcraft/memory/knowledge/backend/fs"

type (
	FSChunkRepo    = target.FSChunkRepo
	FSDocumentRepo = target.FSDocumentRepo
	FSLayerRepo    = target.FSLayerRepo
)

const (
	DefaultPrefix = target.DefaultPrefix
)

var (
	NewChunkRepo    = target.NewChunkRepo
	NewDocumentRepo = target.NewDocumentRepo
	NewLayerRepo    = target.NewLayerRepo
)
