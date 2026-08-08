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
		0,
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
	profile, err := buildProfile(nil, sandbox.NetPolicy{Mode: sandbox.NetDenyAll}, 0)
	if err != nil {
		t.Fatalf("buildProfile: %v", err)
	}
	if !strings.Contains(profile, "(deny network*)") {
		t.Errorf("NetDenyAll profile missing network deny:\n%s", profile)
	}
}

func TestBuildProfile_UnknownNetworkModeRejected(t *testing.T) {
	_, err := buildProfile(nil, sandbox.NetPolicy{Mode: sandbox.NetMode(99)}, 0)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("unknown mode: expected NotAvailable, got %v", err)
	}
}

func TestBuildProfile_AllowListRestrictedNetwork(t *testing.T) {
	profile, err := buildProfile(nil, sandbox.NetPolicy{
		Mode:       sandbox.NetAllowList,
		AllowHosts: []string{"example.com"},
	}, 43123)
	if err != nil {
		t.Fatalf("buildProfile: %v", err)
	}
	// Platform sockets macOS needs for TLS + network config.
	for _, want := range []string{
		"(allow system-socket",
		"(socket-domain AF_SYSTEM)",
		`(global-name "com.apple.SecurityServer")`,
		`(global-name "com.apple.trustd.agent")`,
		`(global-name "com.apple.SystemConfiguration.configd")`,
		"(allow sysctl-read",
		`(sysctl-name-regex #"^net.routetable")`,
		"(deny network*)",
		`(allow network-outbound (remote ip "localhost:43123"))`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("allow_list profile missing %q:\n%s", want, profile)
		}
	}
	// No direct egress beyond the loopback hole.
	if strings.Contains(profile, "(allow network-outbound)\n") {
		t.Errorf("allow_list profile must not open broad outbound:\n%s", profile)
	}
}

func TestBuildProfile_ProxyRestrictedNetwork(t *testing.T) {
	profile, err := buildProfile(nil, sandbox.NetPolicy{
		Mode: sandbox.NetProxy,
	}, 43124)
	if err != nil {
		t.Fatalf("buildProfile: %v", err)
	}
	// Proxy mode uses the same loopback-only hole; the upstream is
	// reached by the host-side proxy, never by the child directly.
	for _, want := range []string{
		"(deny network*)",
		`(allow network-outbound (remote ip "localhost:43124"))`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("proxy profile missing %q:\n%s", want, profile)
		}
	}
}

func TestBuildProfile_RestrictedNetworkRequiresPort(t *testing.T) {
	for _, mode := range []sandbox.NetMode{sandbox.NetAllowList, sandbox.NetProxy} {
		t.Run(strconv.Itoa(int(mode)), func(t *testing.T) {
			_, err := buildProfile(nil, sandbox.NetPolicy{Mode: mode}, 0)
			if !errdefs.IsInternal(err) {
				t.Fatalf("mode %d with port 0: expected Internal, got %v", mode, err)
			}
		})
	}
}
