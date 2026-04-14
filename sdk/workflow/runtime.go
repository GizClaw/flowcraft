package workflow

import "context"

// Runtime executes one agent Request using Strategy + optional Memory.
type Runtime interface {
	Run(ctx context.Context, agent Agent, req *Request, opts ...RunOption) (*Result, error)
}

type runtime struct {
	memoryFactory  MemoryFactory
	deps           *Dependencies
	prepareBoardFn func(ctx context.Context, agent Agent, req *Request, session MemorySession, opts []RunOption) (*Board, error)
}

// NewRuntime constructs a Runtime with optional MemoryFactory and board preparation hook.
func NewRuntime(opts ...RuntimeOption) Runtime {
	rt := &runtime{}
	for _, o := range opts {
		o(rt)
	}
	return rt
}

// Run implements Runtime.
func (rt *runtime) Run(ctx context.Context, agent Agent, req *Request, opts ...RunOption) (*Result, error) {
	return rt.run(ctx, agent, req, opts)
}
