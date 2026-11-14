package runtime

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/permission"
)

func validConfig(root string) Config {
	return Config{
		Version: 1,
		Name:    "test",
		Sandbox: SandboxConfig{Backend: BackendLocal, Writes: []string{root}},
		Files:   FilesConfig{Roots: []string{root}},
	}
}

func TestConfigValidateRejectsMissingBackend(t *testing.T) {
	cfg := validConfig(pathtest.Abs("tmp"))
	cfg.Sandbox.Backend = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("missing backend accepted")
	}
	if !strings.Contains(err.Error(), "sandbox.backend") {
		t.Errorf("error %q does not name the field", err)
	}
}

func TestConfigValidateRejectsUnknownBackend(t *testing.T) {
	cfg := validConfig(pathtest.Abs("tmp"))
	cfg.Sandbox.Backend = "chroot"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

func TestConfigValidateRejectsRelativeRoots(t *testing.T) {
	cfg := validConfig("relative/path")
	if err := cfg.Validate(); err == nil {
		t.Fatal("relative root accepted")
	}
}

// allow_hosts without the matching mode is a configuration that looks like a
// restriction and enforces nothing.
func TestConfigValidateRejectsHostsWithoutAllowList(t *testing.T) {
	cfg := validConfig(pathtest.Abs("tmp"))
	cfg.Sandbox.Net = NetConfig{Mode: "none", AllowHosts: []string{"api.example.com"}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("allow_hosts with mode none accepted")
	}
	if !strings.Contains(err.Error(), "allow_list") {
		t.Errorf("error %q does not explain the mode", err)
	}
}

func TestNetModeNames(t *testing.T) {
	for name, want := range map[string]permission.NetMode{
		"":           permission.NetDefault,
		"default":    permission.NetDefault,
		"none":       permission.NetDenyAll,
		"deny_all":   permission.NetDenyAll,
		"allow_list": permission.NetAllowList,
		"allowlist":  permission.NetAllowList,
		"proxy":      permission.NetProxy,
		"proxies":    permission.NetProxy,
	} {
		got, err := NetConfig{Mode: name}.mode()
		if err != nil {
			t.Errorf("mode %q: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("mode %q = %v, want %v", name, got, want)
		}
	}
	if _, err := (NetConfig{Mode: "vpn"}).mode(); err == nil {
		t.Error("unknown mode accepted")
	}
}

// The written unit is mebibytes; the contract takes bytes.
func TestLimitsConvertMebibytes(t *testing.T) {
	limits, err := SandboxConfig{MemoryMiB: 2048, OutputMiB: 8, CPU: 1500}.limits()
	if err != nil {
		t.Fatalf("limits: %v", err)
	}
	if limits.MemoryBytes != 2048<<20 {
		t.Errorf("memory = %d, want %d", limits.MemoryBytes, int64(2048)<<20)
	}
	if limits.MaxOutputBytes != 8<<20 {
		t.Errorf("output = %d, want %d", limits.MaxOutputBytes, int64(8)<<20)
	}
	if limits.CPUMillicores != 1500 {
		t.Errorf("cpu = %d, want 1500", limits.CPUMillicores)
	}
}

func TestWorkDirDefaultsToFirstRoot(t *testing.T) {
	cfg := Config{Files: FilesConfig{Roots: []string{"/first", "/second"}}}
	if got := cfg.workDir(); got != "/first" {
		t.Errorf("workDir = %q, want the first root", got)
	}
	cfg.Sandbox.WorkDir = "/explicit"
	if got := cfg.workDir(); got != "/explicit" {
		t.Errorf("workDir = %q, want the explicit value", got)
	}
}

// An empty enable list means every set: a runtime with no tools looks complete
// and does nothing.
func TestToolsEnabledDefaultsToAll(t *testing.T) {
	var tools ToolsConfig
	for _, name := range []string{ToolFiles, ToolExec, ToolSkills} {
		if !tools.enabled(name) {
			t.Errorf("%s disabled by an empty enable list", name)
		}
	}
	subset := ToolsConfig{Enable: []string{ToolFiles}}
	if subset.enabled(ToolExec) {
		t.Error("exec enabled although the list names only files")
	}
}

func TestDynamicRejectsUnknownExposure(t *testing.T) {
	cfg := validConfig(pathtest.Abs("tmp"))
	cfg.Tools.Dynamic = &DynamicConfig{Default: "sometimes"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("unknown exposure accepted")
	}
	if !strings.Contains(err.Error(), "tools.dynamic.default") {
		t.Errorf("error %q does not name the field", err)
	}
}

// A tool cannot be always visible and never visible at once.
func TestDynamicRejectsAlwaysAndHiddenOverlap(t *testing.T) {
	cfg := validConfig(pathtest.Abs("tmp"))
	cfg.Tools.Dynamic = &DynamicConfig{Always: []string{"edit"}, Hidden: []string{"edit"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("overlap accepted")
	}
}

// The MCP package owns the transport rules; the runtime must surface them with
// the YAML path, not restate them.
func TestMCPValidationIsDelegated(t *testing.T) {
	cfg := validConfig(pathtest.Abs("tmp"))
	cfg.Tools.MCP = MCPConfig{Servers: []MCPServerConfig{
		{Name: "broken", Transport: "stdio"}, // stdio without a command
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("stdio server without a command accepted")
	}
	if !strings.Contains(err.Error(), "mcp") {
		t.Errorf("error %q does not name the section", err)
	}
}

func TestMCPSpecCarriesOptionalFields(t *testing.T) {
	required := true
	spec, err := MCPConfig{Servers: []MCPServerConfig{{
		Name: "docs", Transport: "http", URL: "https://example.test/mcp",
		Prefix: "docs", Liveness: "30s", Required: &required,
	}}}.spec()
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	server := spec.Servers[0]
	if server.Prefix == nil || *server.Prefix != "docs" {
		t.Errorf("prefix = %v, want docs", server.Prefix)
	}
	if server.Liveness == nil || *server.Liveness != "30s" {
		t.Errorf("liveness = %v, want 30s", server.Liveness)
	}
	if server.Required == nil || !*server.Required {
		t.Errorf("required = %v, want true", server.Required)
	}
}
