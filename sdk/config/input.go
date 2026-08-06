package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/agent"
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
}

// Dep returns the named dependency.
func (in Input) Dep(name string) (any, bool) {
	v, ok := in.Deps[name]
	return v, ok
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
