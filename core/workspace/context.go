package workspace

import "context"

type ctxKey int

const ctxKeyWorkspace ctxKey = iota

func WithWorkspace(ctx context.Context, ws Workspace) context.Context {
	return context.WithValue(ctx, ctxKeyWorkspace, ws)
}

func WorkspaceFrom(ctx context.Context) (Workspace, bool) {
	ws, ok := ctx.Value(ctxKeyWorkspace).(Workspace)
	return ws, ok
}
