package engine

import "context"

// hostCtxKey is the unexported context key under which engines
// stash the [Host] for downstream consumers (built-in tools that
// need to call host.AskUser, custom plugins that emit envelopes
// via host.Publish, …). Using an unexported key prevents
// collision with caller-supplied values and makes the API the
// only legal way in.
type hostCtxKey struct{}

// WithHost returns a derived context carrying h. Engines that
// dispatch to extension points which were not designed to receive
// the Host directly (sdk/tool's Tool.Execute signature, custom
// plugin callbacks, …) call WithHost before invoking those
// extensions so the extension can recover the Host via
// [HostFromContext].
//
// The intended consumer pattern:
//
//	// engine side: just before invoking the tool registry
//	ctx = engine.WithHost(ctx, host)
//	results := reg.ExecuteAll(ctx, calls)
//
//	// tool side
//	host, ok := engine.HostFromContext(ctx)
//	if ok {
//	    reply, err := host.AskUser(ctx, prompt)
//	}
//
// nil h is allowed and a no-op (returns ctx unchanged) so callers
// can plumb a possibly-nil host without conditional branches.
//
// Engines MUST NOT use the host stashed here as a substitute for
// the host argument they receive in [Engine.Execute] — the
// argument is the contract; the context-carried copy is purely a
// transport for downstream extensions that lack a Host parameter.
func WithHost(ctx context.Context, h Host) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, hostCtxKey{}, h)
}

// HostFromContext returns the Host attached to ctx by a previous
// [WithHost] call, plus an "ok" flag. The ok=false branch means
// either no engine wired the host into ctx (the extension is
// running outside an engine) or the caller passed a nil Host.
//
// Extensions that require host capabilities should treat ok=false
// as a usage error and surface it via the extension's own
// error contract (e.g. ask_user surfaces errdefs.NotAvailable so
// LLMs see "I cannot prompt the user" instead of crashing).
func HostFromContext(ctx context.Context) (Host, bool) {
	if ctx == nil {
		return nil, false
	}
	h, ok := ctx.Value(hostCtxKey{}).(Host)
	return h, ok
}
