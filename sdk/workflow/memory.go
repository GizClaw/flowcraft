package workflow

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// Memory provides per-agent session memory.
type Memory interface {
	Session(ctx context.Context, contextID string) (MemorySession, error)
}

// MemorySession is one Run's memory lifecycle (Load → Vars → Save → Close).
// All methods accept a context.Context to support timeout and cancellation
// for implementations backed by databases or network services.
type MemorySession interface {
	Load(ctx context.Context) ([]model.Message, error)
	Vars(ctx context.Context) (map[string]any, error)
	Save(ctx context.Context, messages []model.Message) error
	Close(ctx context.Context, runErr error) error
}

// ContextAssembler is an optional interface that a MemorySession may implement.
// When present, Runtime calls Assemble instead of Load to obtain history messages.
// This allows implementations to perform custom context assembly (e.g. RAG,
// summarization, sliding window) based on the current request.
//
// The returned messages should NOT include req.Message; the runtime appends it.
type ContextAssembler interface {
	Assemble(ctx context.Context, req *Request) ([]model.Message, error)
}

// IncrementalSaver is an optional interface that a MemorySession may implement.
// When present, Runtime calls Append with only the newly produced messages
// instead of calling Save with the full message history.
type IncrementalSaver interface {
	Append(ctx context.Context, newMessages []model.Message) error
}

// MemoryFactory creates a Memory for an agent.
type MemoryFactory func(ctx context.Context, agent Agent) (Memory, error)

// BaseSession is a no-op MemorySession for tests or disabled memory.
type BaseSession struct{}

func (BaseSession) Load(context.Context) ([]model.Message, error) { return nil, nil }
func (BaseSession) Vars(context.Context) (map[string]any, error)  { return nil, nil }
func (BaseSession) Save(context.Context, []model.Message) error   { return nil }
func (BaseSession) Close(context.Context, error) error            { return nil }
