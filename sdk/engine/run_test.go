package engine_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

func TestRun_AttributeReturnsValue(t *testing.T) {
	r := engine.Run{Attributes: map[string]string{"tenant": "acme"}}
	if got := r.Attribute("tenant"); got != "acme" {
		t.Errorf("Attribute(tenant) = %q, want acme", got)
	}
}

func TestRun_AttributeMissingReturnsEmpty(t *testing.T) {
	r := engine.Run{Attributes: map[string]string{"tenant": "acme"}}
	if got := r.Attribute("missing"); got != "" {
		t.Errorf("Attribute(missing) = %q, want empty", got)
	}
}

func TestRun_AttributeNilMapSafe(t *testing.T) {
	r := engine.Run{}
	if got := r.Attribute("any"); got != "" {
		t.Errorf("Attribute on nil map = %q, want empty", got)
	}
}

func TestRun_ZeroValueUsable(t *testing.T) {
	// engine.Run is documented as a plain struct; the zero value
	// must compose correctly with EngineFunc dispatch.
	r := engine.Run{}
	if r.ID != "" {
		t.Errorf("zero ID = %q, want empty", r.ID)
	}
	if r.Deps != nil {
		t.Error("zero Deps must be nil")
	}
	if r.ResumeFrom != nil {
		t.Error("zero ResumeFrom must be nil")
	}
}
