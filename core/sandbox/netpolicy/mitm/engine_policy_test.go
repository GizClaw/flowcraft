package mitm

import (
	"crypto/x509"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/permission"
)

// ShouldMITM decides which CONNECTs are terminated, and it is the only thing
// standing between "inspect this host" and "decrypt everything the sandbox
// talks to". Its selection rules are tested here because the proxy only calls it
// (which a cross-package test cannot attribute).
func TestShouldMITMSelection(t *testing.T) {
	cases := []struct {
		name   string
		policy *permission.MITMPolicy
		host   string
		want   bool
	}{
		{
			name:   "no host list means every host",
			policy: &permission.MITMPolicy{Enabled: true},
			host:   "api.example.com",
			want:   true,
		},
		{
			name:   "a listed host is terminated",
			policy: &permission.MITMPolicy{Enabled: true, Hosts: []string{"api.example.com"}},
			host:   "api.example.com",
			want:   true,
		},
		{
			name:   "a subdomain of a listed host is terminated",
			policy: &permission.MITMPolicy{Enabled: true, Hosts: []string{"example.com"}},
			host:   "api.example.com",
			want:   true,
		},
		{
			name:   "an unlisted host is tunnelled raw",
			policy: &permission.MITMPolicy{Enabled: true, Hosts: []string{"example.com"}},
			host:   "elsewhere.test",
			want:   false,
		},
		{
			name:   "an excluded host is tunnelled even when listed",
			policy: &permission.MITMPolicy{Enabled: true, Hosts: []string{"example.com"}, ExcludeHosts: []string{"private.example.com"}},
			host:   "private.example.com",
			want:   false,
		},
		{
			name:   "an IP literal is matched by an address rule",
			policy: &permission.MITMPolicy{Enabled: true, Hosts: []string{"127.0.0.1"}},
			host:   "127.0.0.1",
			want:   true,
		},
		{
			name:   "an IP outside the CIDR is not terminated",
			policy: &permission.MITMPolicy{Enabled: true, Hosts: []string{"10.0.0.0/8"}},
			host:   "192.168.1.1",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, err := New(tc.policy, nil, nil, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// The engine is an interface; ShouldMITM is part of MITMEngine.
			if got := engine.ShouldMITM(tc.host); got != tc.want {
				t.Fatalf("ShouldMITM(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// The CA the engine publishes is the one a child must trust, so it has to be a
// parseable PEM bundle — and WriteBundle has to put it somewhere the child can
// read and take it away again.
func TestPEMAndBundleCarryTheCA(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows permission bits are advisory: the directory ACL is the boundary")
	}
	engine, err := New(&permission.MITMPolicy{Enabled: true}, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pem := engine.PEM()
	if len(pem) == 0 {
		t.Fatal("PEM() returned nothing")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatalf("PEM() does not parse as a certificate bundle:\n%s", pem)
	}

	path, cleanup, err := WriteBundle(pem)
	if err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if cleanup == nil {
		t.Fatal("WriteBundle returned no cleanup")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("bundle mode = %o, want 600", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	if !strings.Contains(string(body), strings.TrimSpace(string(pem))) {
		t.Error("the bundle does not contain the injected CA")
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup left the bundle behind: %v", err)
	}
}
