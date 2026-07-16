package claw

import memoryhistory "github.com/GizClaw/flowcraft/memory/history"

type options struct {
	historyStore memoryhistory.Store
}

// Option configures a Claw at construction time.
type Option func(*options)

// WithHistoryStore provides the persistence Store used when History is
// enabled. A nil Store is ignored, leaving the Workspace-backed default in
// place. The caller retains ownership of the Store and any resources behind
// it.
func WithHistoryStore(store memoryhistory.Store) Option {
	return func(opts *options) {
		if store != nil {
			opts.historyStore = store
		}
	}
}

func resolveOptions(opts []Option) options {
	var resolved options
	for _, opt := range opts {
		if opt != nil {
			opt(&resolved)
		}
	}
	return resolved
}
