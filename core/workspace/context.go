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

// MustWorkspaceFrom extracts the Workspace from ctx or panics.
// Intended for use in code paths where a Workspace is guaranteed to
// be present (e.g. after middleware injection).
func MustWorkspaceFrom(ctx context.Context) Workspace {
	ws, ok := WorkspaceFrom(ctx)
	if !ok {
		panic("workspace: not found in context")
	}
	return ws
}
