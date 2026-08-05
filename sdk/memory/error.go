package memory

import (
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ErrorKind classifies failures shared by memory capability implementations.
type ErrorKind string

const (
	KindInvalidRequest       ErrorKind = "invalid_request"
	KindNotConfigured        ErrorKind = "not_configured"
	KindConflict             ErrorKind = "conflict"
	KindOperationInterrupted ErrorKind = "operation_interrupted"
	KindProviderFailure      ErrorKind = "provider_failure"
	KindInternal             ErrorKind = "internal"
)

// Error is the implementation-neutral memory error. Capability identifies the
// narrow SPI surface ("context", "turn", or "document") without exposing
// implementation stages or storage details.
type Error struct {
	Kind       ErrorKind
	Capability string
	cause      error
}

func (e *Error) Error() string {
	if e.Capability != "" {
		return fmt.Sprintf("memory: %s capability=%s", e.Kind, e.Capability)
	}
	return fmt.Sprintf("memory: %s", e.Kind)
}

func (e *Error) Unwrap() error { return e.cause }

// NewError classifies cause under both the memory and errdefs taxonomies.
func NewError(kind ErrorKind, capability string, cause error) *Error {
	if cause == nil {
		cause = errors.New(string(kind))
	}
	return &Error{
		Kind:       kind,
		Capability: capability,
		cause:      kind.classify(cause),
	}
}

func (kind ErrorKind) classify(cause error) error {
	switch kind {
	case KindInvalidRequest:
		return errdefs.Validation(cause)
	case KindNotConfigured:
		return errdefs.NotAvailable(cause)
	case KindConflict:
		return errdefs.Conflict(cause)
	case KindOperationInterrupted:
		if classified := errdefs.FromContext(cause); errdefs.HasClassification(classified) {
			if errdefs.IsTimeout(classified) || errdefs.IsAborted(classified) {
				return classified
			}
		}
		return errdefs.Interrupted(cause)
	case KindProviderFailure:
		if classified := errdefs.FromContext(cause); errdefs.HasClassification(classified) {
			return classified
		}
		return errdefs.NotAvailable(cause)
	case KindInternal:
		return errdefs.Internal(cause)
	default:
		return errdefs.Internal(fmt.Errorf("memory: unknown error kind %q: %w", kind, cause))
	}
}

func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

func IsKind(err error, want ErrorKind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == want
}
