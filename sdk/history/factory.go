package history

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	otellog "go.opentelemetry.io/otel/log"
)

// MemoryOption configures optional dependencies for memory creation.
type MemoryOption func(*memoryOptions)

type memoryOptions struct {
	workspace workspace.Workspace
	counter   TokenCounter
	prefix    string
}

// WithWorkspace injects a Workspace (required for lossless).
func WithWorkspace(ws workspace.Workspace) MemoryOption {
	return func(o *memoryOptions) { o.workspace = ws }
}

// WithCounter injects a TokenCounter.
func WithCounter(c TokenCounter) MemoryOption {
	return func(o *memoryOptions) { o.counter = c }
}

// WithPrefix sets the storage prefix for lossless memory files.
func WithPrefix(p string) MemoryOption {
	return func(o *memoryOptions) { o.prefix = p }
}

// NewWithLLM creates a Memory instance. All memory types are unified to lossless;
// deprecated type values (buffer, window, summary, token) emit a warning and
// are treated as lossless. When LLM is nil, lossless degrades to buffer.
//
// Long-term recall composition was removed in v0.2.0: callers wiring an
// [sdk/recall.Memory] alongside this Memory should do so explicitly,
// e.g. by calling recall.Memory.Recall() and prepending hits to the
// system prompt before invoking the LLM. See examples/chatbot-with-recall.
func NewWithLLM(cfg Config, store Store, l llm.LLM, opts ...MemoryOption) (Memory, error) {
	if store == nil {
		store = NewInMemoryStore()
	}

	var o memoryOptions
	for _, opt := range opts {
		opt(&o)
	}

	if cfg.Type != "" && cfg.Type != "lossless" {
		telemetry.Warn(context.Background(), "memory: deprecated type, using lossless", otellog.String("type", cfg.Type))
	}

	return buildCoreMemory(cfg, store, l, o), nil
}

func buildCoreMemory(cfg Config, store Store, l llm.LLM, o memoryOptions) Memory {
	if l == nil {
		telemetry.Warn(context.Background(), "memory: LLM is nil, falling back to buffer")
		return NewBufferMemory(store, cfg.maxMessages())
	}

	ws := o.workspace
	if ws == nil {
		telemetry.Warn(context.Background(), "memory: lossless requires workspace for summary store, falling back to buffer")
		return NewBufferMemory(store, cfg.maxMessages())
	}

	dagCfg := cfg.Lossless.toDAGConfig()
	counter := o.counter
	if counter == nil {
		counter = &EstimateCounter{}
	}
	prefix := o.prefix
	if prefix == "" {
		prefix = "memory"
	}
	summaryStore := NewFileSummaryStore(ws, prefix)
	dag := NewSummaryDAG(summaryStore, store, l, dagCfg, counter)
	return NewLosslessMemory(store, dag, dagCfg, ws, prefix)
}

func (c Config) maxMessages() int {
	if c.MaxMessages > 0 {
		return c.MaxMessages
	}
	return 50
}
