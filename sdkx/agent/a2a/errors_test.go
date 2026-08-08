package a2a

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
)

func TestClassify_Validation(t *testing.T) {
	cases := []error{
		a2aprotocol.ErrParseError,
		a2aprotocol.ErrInvalidRequest,
		a2aprotocol.ErrInvalidParams,
		a2aprotocol.ErrUnsupportedContentType,
	}
	for _, err := range cases {
		got := classify(err)
		if !errdefs.IsValidation(got) {
			t.Errorf("classify(%v) = %v, want Validation", err, got)
		}
		if !errors.Is(got, err) {
			t.Errorf("classify(%v) must still match the sentinel", err)
		}
	}
}

func TestClassify_NotFoundConflictAuth(t *testing.T) {
	if got := classify(a2aprotocol.ErrTaskNotFound); !errdefs.IsNotFound(got) {
		t.Errorf("ErrTaskNotFound -> %v, want NotFound", got)
	}
	if got := classify(a2aprotocol.ErrTaskNotCancelable); !errdefs.IsConflict(got) {
		t.Errorf("ErrTaskNotCancelable -> %v, want Conflict", got)
	}
	if got := classify(a2aprotocol.ErrUnauthenticated); !errdefs.IsUnauthorized(got) {
		t.Errorf("ErrUnauthenticated -> %v, want Unauthorized", got)
	}
	if got := classify(a2aprotocol.ErrUnauthorized); !errdefs.IsForbidden(got) {
		t.Errorf("ErrUnauthorized -> %v, want Forbidden", got)
	}
}

func TestClassify_NotAvailableInternal(t *testing.T) {
	if got := classify(a2aprotocol.ErrMethodNotFound); !errdefs.IsNotAvailable(got) {
		t.Errorf("ErrMethodNotFound -> %v, want NotAvailable", got)
	}
	if got := classify(a2aprotocol.ErrUnsupportedOperation); !errdefs.IsNotAvailable(got) {
		t.Errorf("ErrUnsupportedOperation -> %v, want NotAvailable", got)
	}
	if got := classify(a2aprotocol.ErrInternalError); !errdefs.IsInternal(got) {
		t.Errorf("ErrInternalError -> %v, want Internal", got)
	}
	if got := classify(a2aprotocol.ErrInvalidAgentResponse); !errdefs.IsInternal(got) {
		t.Errorf("ErrInvalidAgentResponse -> %v, want Internal", got)
	}
}

func TestClassify_WrapsA2AError(t *testing.T) {
	// The transport wraps sentinels in *a2a.Error; errors.Is must still
	// classify through the wrapper.
	wrapped := a2aprotocol.NewError(a2aprotocol.ErrTaskNotFound, "nope")
	got := classify(wrapped)
	if !errdefs.IsNotFound(got) {
		t.Errorf("classify(wrapped ErrTaskNotFound) = %v, want NotFound", got)
	}
}

func TestClassify_Passthrough(t *testing.T) {
	ctxErr := context.Canceled
	if got := classify(ctxErr); !errors.Is(got, context.Canceled) {
		t.Errorf("classify(context.Canceled) = %v, want passthrough", got)
	}
	generic := errors.New("boom")
	if got := classify(generic); got != generic {
		t.Errorf("classify(generic) = %v, want unchanged", got)
	}
	if got := classify(nil); got != nil {
		t.Errorf("classify(nil) = %v, want nil", got)
	}
}
