// Package workspace is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/retrieval/workspace instead.
// This compatibility package will be removed in v0.5.0.
package workspace

import (
	"time"

	target "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
)

type (
	Config = target.Config
	Index  = target.Index
	Option = target.Option
)

const (
	DefaultCompactionMaxSize     = target.DefaultCompactionMaxSize
	DefaultCompactionMinSegments = target.DefaultCompactionMinSegments
	DefaultMemtableMaxBytes      = target.DefaultMemtableMaxBytes
	DefaultMemtableMaxDocs       = target.DefaultMemtableMaxDocs
	DefaultWALMaxBytes           = target.DefaultWALMaxBytes
)

var (
	DefaultCompactionInterval time.Duration = target.DefaultCompactionInterval
	DefaultLockHeartbeat      time.Duration = target.DefaultLockHeartbeat
	ErrClosed                               = target.ErrClosed
	ErrCorrupt                              = target.ErrCorrupt
	ErrFenced                               = target.ErrFenced
	ErrLocked                               = target.ErrLocked
	New                                     = target.New
	WithAutoCompact                         = target.WithAutoCompact
	WithClock                               = target.WithClock
	WithCompactionInterval                  = target.WithCompactionInterval
	WithCompactionMaxSize                   = target.WithCompactionMaxSize
	WithCompactionMinSegments               = target.WithCompactionMinSegments
	WithLockHeartbeat                       = target.WithLockHeartbeat
	WithMemtableMaxBytes                    = target.WithMemtableMaxBytes
	WithMemtableMaxDocs                     = target.WithMemtableMaxDocs
	WithTokenizer                           = target.WithTokenizer
	WithWALMaxBytes                         = target.WithWALMaxBytes
)
