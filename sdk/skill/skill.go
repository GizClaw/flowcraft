// Package skill defines discoverable capability-guide contracts.
//
// A skill is a reusable, progressively disclosed guide that teaches an
// agent when and how to use available capabilities. The sdk package
// contains only data models and interfaces; concrete formats and
// loaders (filesystem, workspace, registries) live in sdkx.
package skill

import "context"

// Summary is the compact metadata an agent can use for discovery.
// It should be small enough to show in search results or prompt
// candidates without loading the full guide content.
type Summary struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Source      string   `json:"source,omitempty"`
	Available   bool     `json:"available"`
	MissingDeps []string `json:"missing_deps,omitempty"`
}

// Skill is the full progressively disclosed skill content.
type Skill struct {
	Summary
	Body     string    `json:"body"`
	Path     string    `json:"path,omitempty"`
	Root     string    `json:"root,omitempty"`
	Requires *Requires `json:"requires,omitempty"`
	Gating   *Gating   `json:"gating,omitempty"`
}

// Requires declares runtime requirements discovered from a skill's
// metadata. Loader implementations decide which requirements they can
// evaluate in their environment.
type Requires struct {
	Bins    []string `json:"bins,omitempty"`
	AnyBins []string `json:"any_bins,omitempty"`
	Env     []string `json:"env,omitempty"`
	OS      []string `json:"os,omitempty"`
}

// Gating is the evaluated availability state for a skill.
type Gating struct {
	Available      bool     `json:"available"`
	MissingBins    []string `json:"missing_bins,omitempty"`
	MissingAnyBins []string `json:"missing_any_bins,omitempty"`
	MissingEnv     []string `json:"missing_env,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

// ListOptions controls Catalog.List.
type ListOptions struct {
	Whitelist []string
}

// SearchOptions controls Catalog.Search.
type SearchOptions struct {
	Whitelist []string
	Limit     int
}

// Catalog provides discoverable, progressively disclosed skills.
type Catalog interface {
	List(ctx context.Context, opts ListOptions) ([]Summary, error)
	Search(ctx context.Context, query string, opts SearchOptions) ([]Summary, error)
	Load(ctx context.Context, name string) (*Skill, error)
}
