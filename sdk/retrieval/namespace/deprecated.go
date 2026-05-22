// Package namespace is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/retrieval/namespace instead.
// This compatibility package will be removed in v0.5.0.
package namespace

import target "github.com/GizClaw/flowcraft/memory/retrieval/namespace"

type (
	CopyOptions = target.CopyOptions
	CopyResult  = target.CopyResult
	Prefix      = target.Prefix
)

var (
	CopyNamespace = target.CopyNamespace
	IsValidPrefix = target.IsValidPrefix
	MustRegister  = target.MustRegister
	Register      = target.Register
	Sanitize      = target.Sanitize
)
