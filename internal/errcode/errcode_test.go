package errcode

import (
	"errors"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestResolve_platformError(t *testing.T) {
	err := PluginErrorf("init failed: %v", errors.New("root"))
	code, status := Resolve(err)
	if code != CodePluginError || status != http.StatusUnprocessableEntity {
		t.Fatalf("Resolve = %q %d", code, status)
	}
}

func TestResolve_errdefsFallback(t *testing.T) {
	code, status := Resolve(errdefs.NotFoundf("x"))
	if code != CodeNotFound || status != http.StatusNotFound {
		t.Fatalf("Resolve = %q %d", code, status)
	}
}

func TestFromCode(t *testing.T) {
	e := FromCode(CodeValidationError, "bad input")
	if e.Status() != http.StatusBadRequest || e.Code() != CodeValidationError {
		t.Fatal(e)
	}
}

func TestPublicMessage(t *testing.T) {
	e := Wrap(CodePluginError, 422, errors.New("root"), "wrapper")
	if PublicMessage(e) != "wrapper" {
		t.Fatalf("got %q", PublicMessage(e))
	}
}
