package delegation_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestModeValidate(t *testing.T) {
	for _, mode := range []delegation.Mode{
		delegation.ModeSync,
		delegation.ModeAsync,
	} {
		if err := mode.Validate(); err != nil {
			t.Errorf("%q.Validate() = %v", mode, err)
		}
	}

	err := delegation.Mode("later").Validate()
	if !errdefs.IsValidation(err) {
		t.Fatalf("unknown mode error = %v, want validation classification", err)
	}
}

func TestTargetValidate(t *testing.T) {
	if err := (delegation.Target{ID: "billing", Description: "Billing support"}).Validate(); err != nil {
		t.Fatalf("valid target: %v", err)
	}
	if err := (delegation.Target{}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("empty target error = %v, want validation classification", err)
	}
}

func TestRequestValidate(t *testing.T) {
	valid := delegation.Request{
		Mode:   delegation.ModeAsync,
		Target: "research",
		Input:  "Compare the candidate designs",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	tests := []delegation.Request{
		{Target: "research", Input: "work"},
		{Mode: delegation.ModeSync, Input: "work"},
		{Mode: delegation.ModeSync, Target: "research"},
		{
			Mode:           delegation.ModeSync,
			Target:         "research",
			Input:          "work",
			IdempotencyKey: " \t",
		},
	}
	for _, req := range tests {
		if err := req.Validate(); !errdefs.IsValidation(err) {
			t.Errorf("%+v.Validate() = %v, want validation classification", req, err)
		}
	}
}

func TestRequestIdempotencyKeyJSON(t *testing.T) {
	encoded, err := json.Marshal(delegation.Request{IdempotencyKey: "delivery-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"idempotency_key":"delivery-1"`) {
		t.Fatalf("encoded request = %s", encoded)
	}
	encoded, err = json.Marshal(delegation.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "idempotency_key") {
		t.Fatalf("empty idempotency key was not omitted: %s", encoded)
	}
}

func TestResponseValidate(t *testing.T) {
	if err := (delegation.Response{
		ID:     "delegation-1",
		Status: delegation.StatusSucceeded,
		Output: "done",
	}).Validate(); err != nil {
		t.Fatalf("valid response: %v", err)
	}

	if err := (delegation.Response{ID: "delegation-1", Status: delegation.StatusFailed}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("failed response without error = %v, want validation classification", err)
	}
	if err := (delegation.Response{ID: "delegation-1", Status: delegation.StatusRunning, Output: "early"}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("non-terminal response with output = %v, want validation classification", err)
	}
}

func TestStatusTerminal(t *testing.T) {
	for _, status := range []delegation.Status{
		delegation.StatusSucceeded,
		delegation.StatusFailed,
		delegation.StatusCanceled,
	} {
		if !status.Terminal() {
			t.Errorf("%q.Terminal() = false", status)
		}
	}
	for _, status := range []delegation.Status{
		delegation.StatusAccepted,
		delegation.StatusRunning,
	} {
		if status.Terminal() {
			t.Errorf("%q.Terminal() = true", status)
		}
	}
}

func TestErrorSemantics(t *testing.T) {
	targetErr := delegation.TargetNotFound("missing")
	if !errors.Is(targetErr, delegation.ErrTargetNotFound) || !errdefs.IsNotFound(targetErr) {
		t.Fatalf("TargetNotFound = %v, want sentinel and not-found classification", targetErr)
	}

	requestErr := delegation.RequestNotFound("request-1")
	if !errors.Is(requestErr, delegation.ErrRequestNotFound) || !errdefs.IsNotFound(requestErr) {
		t.Fatalf("RequestNotFound = %v, want sentinel and not-found classification", requestErr)
	}

	modeErr := delegation.UnsupportedMode(delegation.Mode("bogus"))
	if !errors.Is(modeErr, delegation.ErrUnsupportedMode) || !errdefs.IsNotAvailable(modeErr) {
		t.Fatalf("UnsupportedMode = %v, want sentinel and not-available classification", modeErr)
	}
}

type contractDirectory struct{}

func (contractDirectory) List(context.Context) ([]delegation.Target, error) {
	return nil, nil
}

func (contractDirectory) Get(context.Context, string) (delegation.Target, error) {
	return delegation.Target{}, nil
}

type contractService struct{}

func (contractService) Delegate(context.Context, delegation.Request) (delegation.Response, error) {
	return delegation.Response{}, nil
}

func (contractService) Get(context.Context, string) (delegation.Response, error) {
	return delegation.Response{}, nil
}

var (
	_ delegation.Directory = contractDirectory{}
	_ delegation.Service   = contractService{}
)
