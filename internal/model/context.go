package model

import "context"

type ctxKey int

const (
	ctxKeyRuntimeID ctxKey = iota
	ctxKeySandboxHandle
)

// WithRuntimeID injects a runtime ID into the context.
func WithRuntimeID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRuntimeID, id)
}

// RuntimeIDFrom extracts the runtime ID from the context.
func RuntimeIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRuntimeID).(string)
	return id
}

// WithSandboxHandle injects a sandbox handle into the context.
func WithSandboxHandle(ctx context.Context, handle any) context.Context {
	return context.WithValue(ctx, ctxKeySandboxHandle, handle)
}

// SandboxHandleFrom extracts the sandbox handle from the context.
func SandboxHandleFrom(ctx context.Context) any {
	return ctx.Value(ctxKeySandboxHandle)
}
