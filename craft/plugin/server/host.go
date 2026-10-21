package server

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/plugin/protocol"
)

// hostService is the Host service of one connection. It exists per Conn
// rather than once per host for one reason: the connection is the identity,
// so the service that answers a plugin's primitive calls can only ever
// answer for that plugin. There is no token to look up and nothing to
// spoof.
type hostService struct {
	protocol.UnimplementedHostServer
	conn *Conn
}

// Call is the whole host surface: one primitive at a time, named by a
// resource and an op, taking and returning JSON.
//
// There is no method per primitive on purpose. The set of primitives
// belongs to the application (see [Catalog]), so putting it in the proto
// would mean regenerating the wire contract every time an application
// gained a capability — and every plugin would have to be rebuilt against
// a host that speaks a newer minor. Here the envelope is fixed and the
// catalog moves: a host that does not serve a primitive answers NotFound,
// and host.about is how a plugin finds out which ones it may call before
// it calls them.
func (h *hostService) Call(
	ctx context.Context,
	request *protocol.HostCallRequest,
) (*protocol.HostCallResponse, error) {
	resource := strings.TrimSpace(request.GetResource())
	op := strings.TrimSpace(request.GetOp())
	if !validToken(resource) || !validToken(op) {
		return nil, statusOf(errdefs.Validationf(
			"plugin host: primitive %q/%q is not \"resource\" and \"op\"",
			resource, op))
	}
	params, err := decodeParams(request.GetParamsJson())
	if err != nil {
		return nil, statusOf(err)
	}
	out, err := h.invoke(ctx, PrimitiveID(resource, op), params)
	if err != nil {
		return nil, statusOf(err)
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, statusOf(errdefs.Internal(err))
	}
	return &protocol.HostCallResponse{ResultJson: payload}, nil
}

// invoke runs one primitive of the registry under this connection's
// identity: grant check, rate limit, audit, handler.
func (h *hostService) invoke(
	ctx context.Context,
	id string,
	params map[string]any,
) (any, error) {
	server := h.conn.srv
	primitive, ok := server.registry.Lookup(id)
	if !ok {
		return nil, errdefs.NotFoundf("plugin host: %q is not exposed", id)
	}
	identity := Identity{PluginID: h.conn.opts.PluginID, Grants: h.conn.Grants()}
	if primitive.Grant != "" && !identity.Grants.Has(primitive.Grant) {
		return nil, errdefs.Forbiddenf(
			"plugin host: %q requires the %s permission", id, primitive.Grant)
	}
	if !h.conn.allow() {
		return nil, errdefs.RateLimitf("plugin host: rate limit exceeded")
	}
	startedAt := time.Now()
	out, err := primitive.Handler(ctx, Call{Identity: identity, Params: params})
	if server.audit != nil {
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		server.audit(AuditEvent{
			PluginID:  identity.PluginID,
			Primitive: id,
			Outcome:   outcome,
			Duration:  time.Since(startedAt),
		})
	}
	return out, err
}

// decodeParams turns the envelope's opaque arguments into the object the
// handlers read. Nothing is empty and nothing is missing: an absent body is
// nil params, which every handler treats as "no arguments", and a body of
// the wrong shape is refused before a handler sees it.
func decodeParams(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	params := map[string]any{}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, errdefs.Validationf(
			"plugin host: params are not a JSON object: %w", err)
	}
	return params, nil
}

// statusOf renders a primitive failure as the status that carries its class
// across the socket. Without it every host failure would reach a plugin as
// codes.Unknown, and the one thing craft's error classes are for — telling
// a retryable failure from a terminal one — would stop at the socket.
func statusOf(err error) error {
	if err == nil {
		return nil
	}
	code := codes.Unknown
	switch {
	case errdefs.IsValidation(err):
		code = codes.InvalidArgument
	case errdefs.IsNotFound(err):
		code = codes.NotFound
	case errdefs.IsUnauthorized(err), errdefs.IsForbidden(err),
		errdefs.IsPolicyDenied(err):
		code = codes.PermissionDenied
	case errdefs.IsConflict(err), errdefs.IsAborted(err):
		code = codes.FailedPrecondition
	case errdefs.IsTimeout(err):
		code = codes.DeadlineExceeded
	case errdefs.IsRateLimit(err), errdefs.IsBudgetExceeded(err):
		code = codes.ResourceExhausted
	case errdefs.IsNotAvailable(err), errdefs.IsInterrupted(err):
		code = codes.Unavailable
	case errdefs.IsInternal(err):
		code = codes.Internal
	}
	return status.Error(code, err.Error())
}
