package config

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Input is the universal factory input shared by every extension in
// the config protocol: the extension's opaque settings subtree plus
// already-built dependencies.
//
// Extensions decode their own settings with [Opaque.Decode] (or
// [DecodeSettings]) and type-assert dependency values themselves. The
// envelope never interprets either half.
type Input struct {
	// Settings is the extension-owned subtree. Decode it so unknown
	// keys fail the build.
	Settings *Opaque

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
// bytes. It is the single settings-reading helper for resource impls
// that wrap a module's own document loader: the settings subtree is
// itself the document (literal string or structured object) or a
// {file: ...} / {embed: ...} reference.
func (in Input) ResolveDocument(ctx context.Context) ([]byte, error) {
	var src Source
	if err := in.Settings.Decode(&src); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"config: decode settings document: %w", err))
	}
	return in.ResolveSource(ctx, src)
}

// ObserverFactory builds one read-only lifecycle hook. Factories MUST
// decode settings strictly (see [DecodeSettings]) so a typo in YAML
// fails the build rather than silently dropping policy.
type ObserverFactory func(ctx context.Context, in Input) (agent.Observer, error)

// PreparerFactory builds the [agent.Preparer] seed hook.
type PreparerFactory func(ctx context.Context, in Input) (agent.Preparer, error)

// RefereeFactory builds one [agent.Referee] decision hook.
type RefereeFactory func(ctx context.Context, in Input) (agent.Referee, error)

// CommitterFactory builds one [agent.Committer] durable finalizer.
type CommitterFactory func(ctx context.Context, in Input) (agent.Committer, error)
