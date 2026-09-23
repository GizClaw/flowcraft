package journal

import (
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Settings is the deployment shape of the `journal:` subtree. Every
// backend that can watch for writes embeds it under the same key, so
// switching a resource from bwrap to local does not change the
// configuration.
//
// The zero Settings is a valid, fully-enabled journal with every
// implementation default; its presence in the document is the opt-in.
type Settings struct {
	// Exclude lists directories, relative to the runner root, whose
	// subtree is never watched. This is the cost control: a watch costs
	// kernel memory per directory, and a deployment that knows its
	// build trees can keep the journal far away from them.
	Exclude []string `json:"exclude,omitempty"`
	// Ops filters the delivered event classes: create, write, rename,
	// remove. Empty delivers every class.
	Ops []string `json:"ops,omitempty"`
	// Retention is the number of events kept readable for replay.
	// Zero uses the contract default. It is a bounded memory
	// commitment, so absurd values are rejected rather than honoured.
	Retention int `json:"retention,omitempty"`
	// MaxWatchSet caps how many directories are watched, turning an
	// unaffordable watch set into a reported capacity gap instead of a
	// silent hole. Zero uses the platform default.
	MaxWatchSet int `json:"max_watch_set,omitempty"`
}

// Build validates the deployment settings against a runner rooted at
// root and resolves them into engine configuration. Backends call it at
// resource-build time: a deployment that asked for a journal and cannot
// have one — no watch source on this platform, an unusable root, an
// unknown op name — fails the build instead of producing a runner that
// quietly reports nothing.
func (s Settings) Build(root string, extraRoots []string) (Config, error) {
	cfg := Config{Root: root, ExtraRoots: extraRoots}
	for _, name := range s.Ops {
		op, ok := sandbox.ParseFileOp(name)
		if !ok {
			return Config{}, errdefs.Validationf(
				"sandbox journal: unknown op %q (want create, write, rename or remove)", name)
		}
		cfg.Options.Ops = append(cfg.Options.Ops, op)
	}
	if s.Retention < 0 {
		return Config{}, errdefs.Validationf(
			"sandbox journal: retention %d must not be negative", s.Retention)
	}
	cfg.Options.Retention = s.Retention
	if s.MaxWatchSet < 0 {
		return Config{}, errdefs.Validationf(
			"sandbox journal: max_watch_set %d must not be negative", s.MaxWatchSet)
	}
	cfg.Options.MaxWatchSet = s.MaxWatchSet
	cfg.Options.Exclude = s.Exclude

	resolved, err := resolveConfig(cfg)
	if err != nil {
		return Config{}, err
	}
	for _, extra := range extraRoots {
		if _, err := resolveDir(resolved.root, extra); err != nil {
			return Config{}, err
		}
	}
	// The platform comes last: a typo'd op name or an unusable root is
	// a configuration error everywhere, and reporting it as "no watch
	// source here" would hide it.
	if !Available() {
		return Config{}, errdefs.NotAvailablef(
			"sandbox journal: this platform has no file-watch source; remove the journal settings or run on Linux, macOS or Windows")
	}
	return cfg, nil
}
