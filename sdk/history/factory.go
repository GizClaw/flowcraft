package history

import (
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// CompactOption customizes a [Memory] built by [NewCompacted].
//
// Compaction knobs (chunk size, recent ratio, leaf pruning, archive
// threshold, …) used to live in a dedicated Config struct; they are
// now functional options so adding a new knob does not break every
// caller passing a struct literal.
type CompactOption func(*compactOptions)

type compactOptions struct {
	dag     DAGConfig
	counter TokenCounter
	prefix  string
}

// WithDAGConfig overrides the entire [DAGConfig] used by the DAG
// summarizer. Individual knobs below compose on top of a default
// config; use this when you need to set many at once or inherit from a
// [DefaultDAGConfig].
func WithDAGConfig(cfg DAGConfig) CompactOption {
	return func(o *compactOptions) { o.dag = cfg }
}

// WithChunkSize sets how many messages feed into each leaf summary.
func WithChunkSize(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.ChunkSize = n
		}
	}
}

// WithCondenseThreshold sets the sibling-count that triggers a depth+1
// condensation.
func WithCondenseThreshold(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.CondenseThreshold = n
		}
	}
}

// WithMaxDepth caps the summary tree height.
func WithMaxDepth(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.MaxDepth = n
		}
	}
}

// WithTokenBudget caps the assembled context size in estimated tokens.
func WithTokenBudget(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.TokenBudget = n
		}
	}
}

// WithRecentRatio splits the token budget between "recent verbatim
// messages" and "older summaries".
func WithRecentRatio(r float64) CompactOption {
	return func(o *compactOptions) {
		if r > 0 {
			o.dag.RecentRatio = r
		}
	}
}

// WithCompactThreshold triggers compaction once the hot message count
// crosses this number.
func WithCompactThreshold(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.Compact.CompactThreshold = n
		}
	}
}

// WithLeafPrune turns on/off deleting the leaf content after its
// summary is absorbed into a parent node.
func WithLeafPrune(b bool) CompactOption {
	return func(o *compactOptions) { o.dag.Compact.PruneLeafContent = b }
}

// WithArchiveThreshold sets the hot-message count that triggers
// archival of the oldest batch to cold storage.
func WithArchiveThreshold(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.Archive.ArchiveThreshold = n
		}
	}
}

// WithArchiveBatchSize sets how many messages move per archive run.
func WithArchiveBatchSize(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.Archive.ArchiveBatchSize = n
		}
	}
}

// WithTokenCounter swaps the [TokenCounter] used during assembly.
// Defaults to [EstimateCounter].
func WithTokenCounter(c TokenCounter) CompactOption {
	return func(o *compactOptions) {
		if c != nil {
			o.counter = c
		}
	}
}

// WithStoragePrefix sets the workspace prefix for summary/archive
// files. Default "memory" for backwards compatibility with files
// written by prior builds.
func WithStoragePrefix(p string) CompactOption {
	return func(o *compactOptions) {
		if p != "" {
			o.prefix = p
		}
	}
}

// NewCompacted returns a [Memory] that keeps the full transcript but
// summarizes older turns through a DAG to stay within a token budget.
// Requires both an LLM (for summarization) and a [workspace.Workspace]
// (for summary + archive persistence).
//
// This is the recommended default for any agent that holds
// multi-session conversations; use [NewBuffer] for short or
// single-turn interactions.
func NewCompacted(store Store, l llm.LLM, ws workspace.Workspace, opts ...CompactOption) Memory {
	if store == nil {
		store = NewInMemoryStore()
	}
	o := compactOptions{
		dag:     DefaultDAGConfig(),
		counter: &EstimateCounter{},
		prefix:  "memory",
	}
	for _, opt := range opts {
		opt(&o)
	}
	summaryStore := NewFileSummaryStore(ws, o.prefix)
	dag := NewSummaryDAG(summaryStore, store, l, o.dag, o.counter)
	return newCompacted(store, dag, o.dag, ws, o.prefix)
}
