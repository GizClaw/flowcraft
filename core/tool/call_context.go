package tool

import "context"

// callIDCtxKey is the context key for the active tool call id.
//
// The tool executor stamps the id of the message.ToolCall currently
// being executed onto the context handed to the middleware chain and
// the tool implementation, so deep components (a delegate tool, a
// script binding, audit middleware) can correlate a tool's side
// effects with the model-issued call that triggered them.
type callIDCtxKey struct{}

// WithCallID returns a derived context carrying id as the active tool
// call id.
func WithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDCtxKey{}, id)
}

// CallIDFromContext returns the active tool call id carried by ctx.
// ok=false means the caller is outside a tool execution (or the id
// was empty); consumers should treat the zero value as "unknown"
// rather than an error.
func CallIDFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	id, _ := ctx.Value(callIDCtxKey{}).(string)
	return id, id != ""
}
