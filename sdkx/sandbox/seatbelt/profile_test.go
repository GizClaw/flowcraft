package seatbelt

import (
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestBuildProfile_DefaultNetwork(t *testing.T) {
	profile, err := buildProfile(
		[]string{"/workspace", `/tmp/quote"name`},
		sandbox.NetPolicy{Mode: sandbox.NetDefault},
	)
	if err != nil {
		t.Fatalf("buildProfile: %v", err)
	}
	for _, want := range []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (subpath "/workspace"))`,
		`(allow file-write* (subpath "/tmp/quote\"name"))`,
		`(allow file-write* (literal "/dev/null"))`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, "deny network") {
		t.Errorf("NetDefault must not emit a network deny:\n%s", profile)
	}
}

func TestBuildProfile_DenyAllNetwork(t *testing.T) {
	profile, err := buildProfile(nil, sandbox.NetPolicy{Mode: sandbox.NetDenyAll})
	if err != nil {
		t.Fatalf("buildProfile: %v", err)
	}
	if !strings.Contains(profile, "(deny network*)") {
		t.Errorf("NetDenyAll profile missing network deny:\n%s", profile)
	}
}

func TestBuildProfile_UnsupportedNetworkModes(t *testing.T) {
	for _, mode := range []sandbox.NetMode{
		sandbox.NetAllowList,
		sandbox.NetProxy,
		sandbox.NetMode(99),
	} {
		t.Run(strconv.Itoa(int(mode)), func(t *testing.T) {
			_, err := buildProfile(nil, sandbox.NetPolicy{Mode: mode})
			if !errdefs.IsNotAvailable(err) {
				t.Fatalf("mode %d: expected NotAvailable, got %v", mode, err)
			}
		})
	}
}
