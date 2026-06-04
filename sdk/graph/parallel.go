package graph

import "context"

type parallelCtxKey string

const (
	parallelControllerKey parallelCtxKey = "graph.parallel.controller"
)

// ParallelController exposes limited control over the currently executing
// parallel fork. It is intentionally context-scoped: outside a fork it is absent.
type ParallelController interface {
	CancelNode(nodeID, reason string) bool
}

// WithParallelController stores the current fork controller on ctx.
func WithParallelController(ctx context.Context, c ParallelController) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, parallelControllerKey, c)
}

// ParallelControllerFromContext returns the current fork controller, if any.
func ParallelControllerFromContext(ctx context.Context) (ParallelController, bool) {
	c, ok := ctx.Value(parallelControllerKey).(ParallelController)
	return c, ok
}
