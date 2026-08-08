package config

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Input is the universal factory input: the factory-owned settings
// subtree as raw JSON plus already-built dependencies. It is shared by
// every [Factory] in the config protocol.
//
// Factories decode Settings with [DecodeSettings] (strict decoding, so
// unknown keys fail the build) and type-assert dependency values
// themselves. The envelope never interprets either half.
type Input struct {
	// Settings is the factory-owned subtree, strictly decoded by the
	// factory inside New.
	Settings json.RawMessage

	// Deps holds resolved dependencies keyed by the names used in the
	// document.
	Deps map[string]any

	// Resolve materializes a build-time [Source] into bytes — the
	// shared document-resolution capability injected by the assembly
	// host. It may be nil; use [Input.ResolveSource] so a missing
	// resolver becomes a clear validation error instead of a panic.
	Resolve func(ctx context.Context, src Source) ([]byte, error)
}

// Dep returns the named dependency.
func (in Input) Dep(name string) (any, bool) {
	v, ok := in.Deps[name]
	return v, ok
}

// ResolveSource materializes src through the injected resolver. A nil
// resolver is a validation error: a host that accepts Source-typed
// settings must provide the resolution capability.
func (in Input) ResolveSource(ctx context.Context, src Source) ([]byte, error) {
	if in.Resolve == nil {
		return nil, errdefs.Validationf(
			"config: source resolution is not configured; the host must inject Input.Resolve")
	}
	return in.Resolve(ctx, src)
}

// ResolveDocument decodes Settings as a [Source] and materializes it to
// bytes. It is the standard way for a factory whose settings subtree
// is itself a module-owned document (literal string, structured
// content, or {file: ...} / {embed: ...}) to obtain that document.
func (in Input) ResolveDocument(ctx context.Context) ([]byte, error) {
	src, err := DecodeSettings[Source](in.Settings)
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"config: decode settings document: %w", err))
	}
	return in.ResolveSource(ctx, src)
}
