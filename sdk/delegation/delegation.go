package delegation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Mode selects the lifecycle expected by a delegation request.
type Mode string

const (
	// ModeSync waits for the delegated work and returns its terminal response.
	ModeSync Mode = "sync"
	// ModeHandoff transfers ownership of the current interaction.
	ModeHandoff Mode = "handoff"
	// ModeAsync admits work and returns before it reaches a terminal state.
	ModeAsync Mode = "async"
)

// Validate checks that m is a defined delegation mode.
func (m Mode) Validate() error {
	switch m {
	case ModeSync, ModeHandoff, ModeAsync:
		return nil
	default:
		return errdefs.Validationf("delegation: unknown mode %q", m)
	}
}

// Target describes a backend-neutral delegation destination. Modes is an
// explicit allowlist; an empty Modes list means unrestricted support for every
// defined delegation mode.
type Target struct {
	ID          string            `json:"id"`
	Description string            `json:"description,omitempty"`
	Modes       []Mode            `json:"modes,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Validate checks the target identity and any advertised modes.
func (t Target) Validate() error {
	if strings.TrimSpace(t.ID) == "" {
		return errdefs.Validationf("delegation: target id is required")
	}
	seen := make(map[Mode]struct{}, len(t.Modes))
	for _, mode := range t.Modes {
		if err := mode.Validate(); err != nil {
			return err
		}
		if _, ok := seen[mode]; ok {
			return errdefs.Validationf("delegation: target %q repeats mode %q", t.ID, mode)
		}
		seen[mode] = struct{}{}
	}
	return nil
}

// Request is the portable input accepted by a Service.
type Request struct {
	Mode           Mode              `json:"mode"`
	Target         string            `json:"target"`
	Input          string            `json:"input"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

// Validate checks fields required by every delegation backend.
func (r Request) Validate() error {
	if err := r.Mode.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Target) == "" {
		return errdefs.Validationf("delegation: request target is required")
	}
	if strings.TrimSpace(r.Input) == "" {
		return errdefs.Validationf("delegation: request input is required")
	}
	if r.IdempotencyKey != "" && strings.TrimSpace(r.IdempotencyKey) == "" {
		return errdefs.Validationf("delegation: request idempotency key must not be blank")
	}
	return nil
}

// Status describes the backend-neutral lifecycle of delegated work.
type Status string

const (
	StatusAccepted  Status = "accepted"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// Validate checks that s is a defined delegation status.
func (s Status) Validate() error {
	switch s {
	case StatusAccepted, StatusRunning, StatusSucceeded, StatusFailed, StatusCanceled:
		return nil
	default:
		return errdefs.Validationf("delegation: unknown status %q", s)
	}
}

// Terminal reports whether no further status transition is expected.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled:
		return true
	default:
		return false
	}
}

// Response is a snapshot of delegated work.
type Response struct {
	ID       string            `json:"id"`
	Status   Status            `json:"status"`
	Output   string            `json:"output,omitempty"`
	Error    string            `json:"error,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Validate checks lifecycle-dependent response invariants.
func (r Response) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return errdefs.Validationf("delegation: response id is required")
	}
	if err := r.Status.Validate(); err != nil {
		return err
	}
	if !r.Status.Terminal() && (r.Output != "" || r.Error != "") {
		return errdefs.Validationf("delegation: non-terminal response %q cannot carry output or error", r.Status)
	}
	switch r.Status {
	case StatusSucceeded:
		if r.Error != "" {
			return errdefs.Validationf("delegation: succeeded response cannot carry an error")
		}
	case StatusFailed:
		if strings.TrimSpace(r.Error) == "" {
			return errdefs.Validationf("delegation: failed response requires an error")
		}
	case StatusCanceled:
		if r.Output != "" {
			return errdefs.Validationf("delegation: canceled response cannot carry output")
		}
	}
	return nil
}

// Directory discovers delegation targets independently of execution.
type Directory interface {
	List(ctx context.Context) ([]Target, error)
	Get(ctx context.Context, id string) (Target, error)
}

// Service starts delegated work and retrieves its latest response.
// Implementations must declare and guarantee a finite idempotency retention
// window. Within that window, requests with the same non-empty idempotency key
// and business fields must safely replay the same operation, and its response
// must remain queryable; reusing the key for a semantically different request
// must return a conflict-classified error. After the declared window expires,
// the key may start a new operation: this contract is bounded idempotency, not
// permanent exactly-once execution.
type Service interface {
	Delegate(ctx context.Context, req Request) (Response, error)
	Get(ctx context.Context, id string) (Response, error)
}

var (
	// ErrTargetNotFound identifies an unknown target.
	ErrTargetNotFound = errors.New("delegation: target not found")
	// ErrRequestNotFound identifies an unknown delegation request.
	ErrRequestNotFound = errors.New("delegation: request not found")
	// ErrUnsupportedMode identifies a valid mode unsupported by a backend.
	ErrUnsupportedMode = errors.New("delegation: mode not supported")
)

// TargetNotFound returns a not-found-classified target lookup error.
func TargetNotFound(id string) error {
	return errdefs.NotFound(fmt.Errorf("%w: %s", ErrTargetNotFound, id))
}

// RequestNotFound returns a not-found-classified request lookup error.
func RequestNotFound(id string) error {
	return errdefs.NotFound(fmt.Errorf("%w: %s", ErrRequestNotFound, id))
}

// UnsupportedMode returns a not-available-classified backend capability error.
func UnsupportedMode(mode Mode) error {
	return errdefs.NotAvailable(fmt.Errorf("%w: %s", ErrUnsupportedMode, mode))
}
