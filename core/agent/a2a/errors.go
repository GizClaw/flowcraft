package a2a

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/core/errdefs"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
)

// classify maps an A2A transport error onto the errdefs classification so
// callers (and agent.Execute) can branch with errdefs.IsXxx. The mapping
// mirrors the A2A error-code table in the package plan:
//
//   - protocol-shape errors (parse / invalid request / invalid params /
//     unsupported content type) -> Validation;
//   - capability gaps (method not found, unsupported operation, push
//     notifications unsupported, version unsupported, extension required)
//     -> NotAvailable;
//   - task not found -> NotFound;
//   - task not cancelable / concurrent modification -> Conflict;
//   - unauthenticated -> Unauthorized; unauthorized -> Forbidden;
//   - internal / server / invalid-agent-response -> Internal.
//
// Context cancellation and deadline errors pass through untouched so the
// engine's cancellation contract stays exact, and unrecognised errors are
// returned as-is (the engine surfaces them as a failed run rather than
// misclassifying them).
func classify(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, a2aprotocol.ErrParseError),
		errors.Is(err, a2aprotocol.ErrInvalidRequest),
		errors.Is(err, a2aprotocol.ErrInvalidParams),
		errors.Is(err, a2aprotocol.ErrUnsupportedContentType):
		return errdefs.Validation(err)
	case errors.Is(err, a2aprotocol.ErrMethodNotFound),
		errors.Is(err, a2aprotocol.ErrUnsupportedOperation),
		errors.Is(err, a2aprotocol.ErrPushNotificationNotSupported),
		errors.Is(err, a2aprotocol.ErrVersionNotSupported),
		errors.Is(err, a2aprotocol.ErrExtensionSupportRequired):
		return errdefs.NotAvailable(err)
	case errors.Is(err, a2aprotocol.ErrTaskNotFound):
		return errdefs.NotFound(err)
	case errors.Is(err, a2aprotocol.ErrTaskNotCancelable):
		return errdefs.Conflict(err)
	case errors.Is(err, a2aprotocol.ErrUnauthenticated):
		return errdefs.Unauthorized(err)
	case errors.Is(err, a2aprotocol.ErrUnauthorized):
		return errdefs.Forbidden(err)
	case errors.Is(err, a2aprotocol.ErrInternalError),
		errors.Is(err, a2aprotocol.ErrServerError),
		errors.Is(err, a2aprotocol.ErrInvalidAgentResponse):
		return errdefs.Internal(err)
	default:
		return err
	}
}
