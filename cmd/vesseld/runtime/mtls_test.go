package runtime

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
)

func TestApplyMTLSOverrides_NoFlags_NoOp(t *testing.T) {
	t.Parallel()
	plan := &resolver.Plan{}
	if err := applyMTLSOverrides(plan, RunOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Daemon.MTLS != nil {
		t.Fatalf("MTLS unexpectedly populated: %+v", plan.Daemon.MTLS)
	}
}

func TestApplyMTLSOverrides_AllFromCLI(t *testing.T) {
	t.Parallel()
	plan := &resolver.Plan{}
	err := applyMTLSOverrides(plan, RunOptions{
		MTLSCert:     "file:///cert",
		MTLSKey:      "file:///key",
		MTLSClientCA: "file:///ca",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := plan.Daemon.MTLS
	if m == nil {
		t.Fatal("MTLS not populated")
	}
	if m.CertRef != "file:///cert" || m.KeyRef != "file:///key" || m.ClientCARef != "file:///ca" {
		t.Fatalf("refs not propagated: %+v", m)
	}
	if m.MinVersion != "1.3" {
		t.Fatalf("default MinVersion = %q, want 1.3", m.MinVersion)
	}
}

func TestApplyMTLSOverrides_PartialCLI_FailsWhenNoYAML(t *testing.T) {
	t.Parallel()
	plan := &resolver.Plan{}
	err := applyMTLSOverrides(plan, RunOptions{MTLSCert: "file:///cert"})
	if err == nil || !strings.Contains(err.Error(), "all of --cert") {
		t.Fatalf("expected partial-input error, got %v", err)
	}
}

func TestApplyMTLSOverrides_PartialCLI_MergesWithYAML(t *testing.T) {
	t.Parallel()
	plan := &resolver.Plan{
		Daemon: resolver.DaemonPlan{
			MTLS: &resolver.DaemonMTLSPlan{
				CertRef:     "file:///yaml-cert",
				KeyRef:      "file:///yaml-key",
				ClientCARef: "file:///yaml-ca",
				MinVersion:  "1.3",
			},
		},
	}
	err := applyMTLSOverrides(plan, RunOptions{MTLSCert: "file:///cli-cert"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := plan.Daemon.MTLS
	if m.CertRef != "file:///cli-cert" {
		t.Fatalf("CertRef override missed: %q", m.CertRef)
	}
	if m.KeyRef != "file:///yaml-key" {
		t.Fatalf("KeyRef should retain yaml value, got %q", m.KeyRef)
	}
	if m.ClientCARef != "file:///yaml-ca" {
		t.Fatalf("ClientCARef should retain yaml value, got %q", m.ClientCARef)
	}
	if m.MinVersion != "1.3" {
		t.Fatalf("MinVersion clobbered: %q", m.MinVersion)
	}
}
