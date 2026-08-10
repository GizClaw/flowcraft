package sandbox_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestNetPolicy_Validate(t *testing.T) {
	valid := sandbox.NetPolicy{
		Mode: sandbox.NetAllowList,
		Rules: []sandbox.NetRule{
			{Action: sandbox.NetDeny, Host: "*.internal.example", Port: 0},
			{Action: sandbox.NetAllow, Host: "example.com", Port: 443},
			{Action: sandbox.NetAllow, Host: "10.0.0.0/8", Port: 0},
		},
		Proxy:       "socks5://user:pass@proxy.example:1080",
		UnixSockets: []string{"/run/docker.sock"},
		MITM: &sandbox.MITMPolicy{
			Enabled:      true,
			MaxBodyBytes: 65536,
			Hosts:        []string{"example.com", "*.pinned.internal"},
			ExcludeHosts: []string{"*.nopin.example"},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}

	cases := []struct {
		name string
		pol  sandbox.NetPolicy
	}{
		{"bad scheme", sandbox.NetPolicy{Proxy: "ftp://x"}},
		{"password without user", sandbox.NetPolicy{Proxy: "socks5://:pass@host:1080"}},
		{"bad action", sandbox.NetPolicy{Rules: []sandbox.NetRule{{Action: sandbox.NetAction(9), Host: "x"}}}},
		{"empty rule host", sandbox.NetPolicy{Rules: []sandbox.NetRule{{Action: sandbox.NetAllow, Host: " "}}}},
		{"port out of range", sandbox.NetPolicy{Rules: []sandbox.NetRule{{Action: sandbox.NetAllow, Host: "x", Port: 70000}}}},
		{"relative unix socket", sandbox.NetPolicy{UnixSockets: []string{"run/docker.sock"}}},
		{"negative body cap", sandbox.NetPolicy{MITM: &sandbox.MITMPolicy{MaxBodyBytes: -1}}},
		{"empty mitm host", sandbox.NetPolicy{MITM: &sandbox.MITMPolicy{Hosts: []string{" "}}}},
		{"empty mitm exclude", sandbox.NetPolicy{MITM: &sandbox.MITMPolicy{ExcludeHosts: []string{""}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.pol.Validate(); !errdefs.IsValidation(err) {
				t.Fatalf("Validate = %v, want Validation", err)
			}
		})
	}
}
