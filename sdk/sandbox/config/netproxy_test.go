package config_test

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	sandboxconfig "github.com/GizClaw/flowcraft/sdk/sandbox/config"
)

func TestParseNetProxyEnhancements(t *testing.T) {
	doc := mustParse(t, `
version: v1
sandboxes:
  x:
    backend: local
    workspace: project
    defaults:
      net:
        mode: allow_list
        rules:
          - action: deny
            host: "*.internal.example"
          - action: allow
            host: "example.com"
            port: 443
          - action: allow
            host: "10.0.0.0/8"
        unix_sockets: [/run/docker.sock]
        mitm:
          enabled: true
          inspect_bodies: true
          max_body_bytes: 65536
          hosts: ["example.com"]
          exclude_hosts: ["*.nopin.example"]
`)
	net := doc.Sandboxes["x"].Defaults.Net
	if len(net.Rules) != 3 || net.Rules[0].Action != "deny" || net.Rules[1].Port != 443 {
		t.Fatalf("rules = %+v", net.Rules)
	}
	if len(net.UnixSockets) != 1 || net.UnixSockets[0] != "/run/docker.sock" {
		t.Fatalf("unix_sockets = %v", net.UnixSockets)
	}
	if net.MITM == nil || !net.MITM.Enabled || !net.MITM.InspectBodies || net.MITM.MaxBodyBytes != 65536 {
		t.Fatalf("mitm = %+v", net.MITM)
	}
	if len(net.MITM.Hosts) != 1 || net.MITM.Hosts[0] != "example.com" {
		t.Fatalf("mitm.hosts = %v", net.MITM.Hosts)
	}
	if len(net.MITM.ExcludeHosts) != 1 || net.MITM.ExcludeHosts[0] != "*.nopin.example" {
		t.Fatalf("mitm.exclude_hosts = %v", net.MITM.ExcludeHosts)
	}

	proxyDoc := mustParse(t, `
version: v1
sandboxes:
  x:
    backend: local
    workspace: project
    defaults:
      net:
        mode: proxy
        proxy: socks5://user:pass@proxy.example:1080
`)
	if got := proxyDoc.Sandboxes["x"].Defaults.Net.Proxy; got != "socks5://user:pass@proxy.example:1080" {
		t.Fatalf("proxy = %q", got)
	}
}

func TestParseRejectsInvalidNetProxyEnhancements(t *testing.T) {
	tests := map[string]string{
		"bad action":                        `{backend: local, workspace: project, defaults: {net: {mode: allow_list, rules: [{action: maybe, host: x}]}}}`,
		"bad port":                          `{backend: local, workspace: project, defaults: {net: {mode: allow_list, rules: [{action: allow, host: x, port: 70000}]}}}`,
		"empty rule host":                   `{backend: local, workspace: project, defaults: {net: {mode: allow_list, rules: [{action: allow}]}}}`,
		"bad scheme":                        `{backend: local, workspace: project, defaults: {net: {mode: proxy, proxy: "ftp://x"}}}`,
		"relative socket":                   `{backend: local, workspace: project, defaults: {net: {mode: allow_list, allow_hosts: [x], unix_sockets: [run/docker.sock]}}}`,
		"negative mitm":                     `{backend: local, workspace: project, defaults: {net: {mode: allow_list, allow_hosts: [x], mitm: {max_body_bytes: -1}}}}`,
		"rules in default mode":             `{backend: local, workspace: project, defaults: {net: {mode: default, rules: [{action: allow, host: x}]}}}`,
		"allow_list without hosts or rules": `{backend: local, workspace: project, defaults: {net: {mode: allow_list}}}`,
	}
	for name, entry := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := sandboxconfig.Parse([]byte("version: v1\nsandboxes:\n  x: " + entry + "\n"))
			if !errdefs.IsValidation(err) {
				t.Fatalf("Validate = %v, want Validation", err)
			}
		})
	}
}

func TestBuilderGatesProxyCapabilities(t *testing.T) {
	root := t.TempDir()
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{Workspaces: buildWorkspaces(t, root, false)})
	runner := &proxyCapsRunner{
		caps: sandbox.Enforcement{
			EnvAllowList: true,
			NetModes:     []sandbox.NetMode{sandbox.NetAllowList, sandbox.NetProxy},
		},
	}
	if err := builder.RegisterFactory("proxycaps", func(context.Context, sandboxconfig.FactoryInput) (sandbox.Runner, error) {
		return runner, nil
	}); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}

	tests := map[string]struct {
		net   string
		cap   string
		field string
	}{
		"socks5": {
			net:   `{mode: proxy, proxy: "socks5://user:pass@host:1080"}`,
			cap:   "Socks5",
			field: "socks5 upstream",
		},
		"mitm": {
			net:   `{mode: allow_list, allow_hosts: [example.com], mitm: {enabled: true}}`,
			cap:   "MITM",
			field: "MITM",
		},
		"unix sockets": {
			net:   `{mode: allow_list, allow_hosts: [example.com], unix_sockets: [/run/docker.sock]}`,
			cap:   "UnixSocketPolicy",
			field: "unix socket allow-list",
		},
	}
	for name, tc := range tests {
		t.Run(name+" unsupported", func(t *testing.T) {
			doc := mustParse(t, "version: v1\nsandboxes:\n  x:\n    backend: proxycaps\n    workspace: project\n    defaults:\n      net: "+tc.net+"\n")
			_, err := builder.Build(context.Background(), doc)
			if !errdefs.IsNotAvailable(err) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("Build = %v, want NotAvailable containing %q", err, tc.field)
			}
		})
		t.Run(name+" supported", func(t *testing.T) {
			runner.caps = sandbox.Enforcement{
				EnvAllowList: true,
				NetModes:     []sandbox.NetMode{sandbox.NetAllowList, sandbox.NetProxy},
			}
			switch tc.cap {
			case "Socks5":
				runner.caps.Socks5 = true
			case "MITM":
				runner.caps.MITM = true
			case "UnixSocketPolicy":
				runner.caps.UnixSocketPolicy = true
			}
			doc := mustParse(t, "version: v1\nsandboxes:\n  x:\n    backend: proxycaps\n    workspace: project\n    defaults:\n      net: "+tc.net+"\n")
			if _, err := builder.Build(context.Background(), doc); err != nil {
				t.Fatalf("Build with capability = %v, want success", err)
			}
		})
	}
}

func TestParseApprovalInteractive(t *testing.T) {
	doc := mustParse(t, `
version: v1
sandboxes:
  x:
    backend: local
    workspace: project
    approval: {interactive: true}
`)
	if doc.Sandboxes["x"].Approval == nil || !doc.Sandboxes["x"].Approval.Interactive {
		t.Fatalf("approval = %+v, want interactive: true", doc.Sandboxes["x"].Approval)
	}
}

func TestApproval_InteractiveRequiresApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("interactive sessions are unix-only")
	}
	root := t.TempDir()
	var calls int
	approver := func(_ context.Context, req sandbox.ApprovalRequest) (sandbox.Decision, error) {
		calls++
		if !req.Exec.TTY {
			t.Errorf("approver saw TTY=%v, want true", req.Exec.TTY)
		}
		return sandbox.Allow, nil
	}
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{
		Workspaces: buildWorkspaces(t, root, false),
		Approver:   approver,
	})
	doc := mustParse(t, `
version: v1
sandboxes:
  x:
    backend: local
    workspace: project
    approval: {interactive: true}
`)
	registry, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	runner, ok := registry.Get("x")
	if !ok || runner == nil {
		t.Fatal("sandbox x not resolved")
	}
	pm := sandbox.ProcessManagerOf(runner)
	if pm == nil {
		t.Fatal("composed runner must forward ProcessManager")
	}

	proc, err := pm.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/usr/bin/true"},
		TTY:  true,
	})
	if err != nil {
		t.Fatalf("TTY Start: %v", err)
	}
	defer func() { _ = proc.Close() }()
	if calls != 1 {
		t.Fatalf("approver calls = %d, want 1 for TTY start", calls)
	}

	proc, err = pm.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/usr/bin/true"},
	})
	if err != nil {
		t.Fatalf("pipe Start: %v", err)
	}
	defer func() { _ = proc.Close() }()
	if calls != 1 {
		t.Fatalf("pipe start must not trip Interactive: calls = %d", calls)
	}
}

type proxyCapsRunner struct {
	caps sandbox.Enforcement
}

func (r *proxyCapsRunner) Enforcement() sandbox.Enforcement {
	return r.caps
}

func (r *proxyCapsRunner) Exec(context.Context, string, []string, sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	return &sandbox.ExecResult{}, nil
}
