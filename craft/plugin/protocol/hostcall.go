package protocol

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc/codes"
	gstatus "google.golang.org/grpc/status"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Invoke runs one host primitive through host and decodes its JSON answer
// into out. params is marshalled into the request (a nil params sends {}),
// and a nil out discards the answer.
//
// The primitive is named the way host.about names it: a resource and an op.
// Invoke exists so that a plugin never hand-builds the envelope — the wire
// has one method, and this is the one place that knows how to call it.
func Invoke(
	ctx context.Context,
	host HostClient,
	resource, op string,
	params, out any,
) error {
	if host == nil {
		return errdefs.NotAvailablef("plugin: no host connection")
	}
	body := []byte(nil)
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return errdefs.Validationf(
				"plugin: %s.%s params: %w", resource, op, err)
		}
		body = encoded
	}
	response, err := host.Call(ctx, &HostCallRequest{
		Resource:   resource,
		Op:         op,
		ParamsJson: body,
	})
	if err != nil {
		return FromStatus(err)
	}
	if out == nil {
		return nil
	}
	payload := response.GetResultJson()
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return errdefs.Internalf(
			"plugin: %s.%s answered %s: %w", resource, op, payload, err)
	}
	return nil
}

// FromStatus turns the status a host call failed with back into an error of
// the class it carried, so a plugin reasons about a host failure exactly the
// way it reasons about a local one.
func FromStatus(err error) error {
	if err == nil {
		return nil
	}
	status, ok := gstatus.FromError(err)
	if !ok {
		return err
	}
	switch status.Code() {
	case codes.OK:
		return nil
	case codes.InvalidArgument:
		return errdefs.Validation(errString(status.Message()))
	case codes.NotFound:
		return errdefs.NotFound(errString(status.Message()))
	case codes.Unauthenticated, codes.PermissionDenied:
		return errdefs.Forbidden(errString(status.Message()))
	case codes.FailedPrecondition, codes.Aborted:
		return errdefs.Aborted(errString(status.Message()))
	case codes.DeadlineExceeded:
		return errdefs.Timeout(errString(status.Message()))
	case codes.ResourceExhausted:
		return errdefs.RateLimit(errString(status.Message()))
	case codes.Unavailable:
		return errdefs.NotAvailable(errString(status.Message()))
	case codes.Canceled:
		return errdefs.FromContext(context.Canceled)
	default:
		return errdefs.Internal(errString(status.Message()))
	}
}
