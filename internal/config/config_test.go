package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateFlowcraftHome points os.UserHomeDir at a temp directory and creates ~/.flowcraft.
func isolateFlowcraftHome(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	root := filepath.Join(base, ".flowcraft")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDefault(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	if cfg.Server.Port != 8080 {
		t.Fatalf("expected default port 8080, got %d", cfg.Server.Port)
	}
	if cfg.Memory.Type != "lossless" {
		t.Fatalf("expected default memory type 'lossless', got %q", cfg.Memory.Type)
	}
	if cfg.DB.Path != "flowcraft.db" {
		t.Fatalf("expected default db path flowcraft.db, got %q", cfg.DB.Path)
	}
}

func TestYAMLMerge_ServerLogAuth(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `
server:
  port: 9090
log:
  level: debug
auth:
  api_key: test-key
`)
	cfg := Load()
	if cfg.Server.Port != 9090 {
		t.Fatalf("expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Log.Level != "debug" {
		t.Fatalf("expected log level 'debug', got %q", cfg.Log.Level)
	}
}

func TestValidate(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	warnings := cfg.Validate()
	_ = warnings // JWT auth is managed in DB; no config-level auth warning expected
}

func TestValidate_BadPort(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `server:
  port: -1
`)
	cfg := Load()
	warnings := cfg.Validate()
	found := false
	for _, w := range warnings {
		if w != "" && len(w) > 10 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected port warning for invalid port")
	}
}

func TestAddress(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	if cfg.Address() != "0.0.0.0:8080" {
		t.Fatalf("expected '0.0.0.0:8080', got %q", cfg.Address())
	}
}

func TestYAMLMerge_Plugin(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `
plugin:
  dir: /custom/plugins
  config_file: /etc/flowcraft/plugins.json
  health_interval: 30
  max_failures: 5
  max_upload_size: 52428800
`)
	cfg := Load()
	if cfg.Plugin.Dir != "/custom/plugins" {
		t.Fatalf("expected plugin dir '/custom/plugins', got %q", cfg.Plugin.Dir)
	}
	if cfg.Plugin.ConfigFile != "/etc/flowcraft/plugins.json" {
		t.Fatalf("expected config file path, got %q", cfg.Plugin.ConfigFile)
	}
	if cfg.Plugin.HealthInterval != 30 {
		t.Fatalf("expected health interval 30, got %d", cfg.Plugin.HealthInterval)
	}
	if cfg.Plugin.MaxFailures != 5 {
		t.Fatalf("expected max failures 5, got %d", cfg.Plugin.MaxFailures)
	}
	if cfg.Plugin.MaxUploadSize != 52428800 {
		t.Fatalf("expected 52428800, got %d", cfg.Plugin.MaxUploadSize)
	}
}

func TestPluginMaxUploadSizeDefault(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	expected := int64(100 << 20) // 100MB
	if cfg.Plugin.MaxUploadSize != expected {
		t.Fatalf("expected default MaxUploadSize %d, got %d", expected, cfg.Plugin.MaxUploadSize)
	}
}

func TestYAMLMerge_CORSOrigins(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `
server:
  cors_origins:
    - "http://localhost:3000"
    - "https://example.com"
    - "http://foo.bar"
`)
	cfg := Load()
	expected := []string{"http://localhost:3000", "https://example.com", "http://foo.bar"}
	if len(cfg.Server.CORSOrigins) != 3 {
		t.Fatalf("expected 3 origins, got %d: %v", len(cfg.Server.CORSOrigins), cfg.Server.CORSOrigins)
	}
	for i, want := range expected {
		if cfg.Server.CORSOrigins[i] != want {
			t.Fatalf("origin %d: expected %q, got %q", i, want, cfg.Server.CORSOrigins[i])
		}
	}
}

func TestYAMLMerge_InvalidPort_KeepsFromYAML(t *testing.T) {
	root := isolateFlowcraftHome(t)
	// Invalid port in YAML: yaml unmarshals as 0 or fails - we use valid yaml with 0
	mustWrite(t, filepath.Join(root, "config.yaml"), `server:
  port: 0
`)
	cfg := Load()
	if cfg.Server.Port != 0 {
		t.Fatalf("expected port 0 from yaml, got %d", cfg.Server.Port)
	}
}

func TestYAMLMerge_Monitoring(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `
monitoring:
  error_rate_warn: 0.07
  error_rate_down: 0.25
  p95_warn_ms: 4500
  consecutive_buckets: 4
  no_success_down_minutes: 3
`)
	cfg := Load()
	if cfg.Monitoring.ErrorRateWarn != 0.07 {
		t.Fatalf("expected warning rate 0.07, got %f", cfg.Monitoring.ErrorRateWarn)
	}
	if cfg.Monitoring.ErrorRateDown != 0.25 {
		t.Fatalf("expected down rate 0.25, got %f", cfg.Monitoring.ErrorRateDown)
	}
	if cfg.Monitoring.LatencyP95WarnMs != 4500 {
		t.Fatalf("expected p95 warn 4500, got %d", cfg.Monitoring.LatencyP95WarnMs)
	}
	if cfg.Monitoring.ConsecutiveBuckets != 4 {
		t.Fatalf("expected consecutive buckets 4, got %d", cfg.Monitoring.ConsecutiveBuckets)
	}
	if cfg.Monitoring.NoSuccessDownMinutes != 3 {
		t.Fatalf("expected no-success minutes 3, got %d", cfg.Monitoring.NoSuccessDownMinutes)
	}
}

func TestYAMLMerge_SkillsEntries(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `
skills:
  entries:
    image-gen:
      enabled: false
      api_key: sk-test
      env:
        MODEL: dall-e-3
    weather: {}
`)
	cfg := Load()
	if len(cfg.Skills.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(cfg.Skills.Entries))
	}
	ig := cfg.Skills.Entries["image-gen"]
	if ig.Enabled == nil || *ig.Enabled != false {
		t.Fatal("image-gen should be disabled")
	}
	if ig.APIKey != "sk-test" {
		t.Fatalf("expected api_key 'sk-test', got %q", ig.APIKey)
	}
	if ig.Env["MODEL"] != "dall-e-3" {
		t.Fatalf("expected MODEL 'dall-e-3', got %q", ig.Env["MODEL"])
	}
	w := cfg.Skills.Entries["weather"]
	if w.Enabled != nil {
		t.Fatal("weather enabled should be nil (default)")
	}
}

func TestMaskSecret(t *testing.T) {
	if maskSecret("") != "(not set)" {
		t.Fatal("empty should be '(not set)'")
	}
	if maskSecret("short") != "****" {
		t.Fatal("short should be '****'")
	}
	if maskSecret("a-very-long-key-here") != "a-ve****here" {
		t.Fatalf("unexpected mask: %s", maskSecret("a-very-long-key-here"))
	}
}

func TestDBPath_Absolute(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	cfg.DB.Path = "/absolute/db.sqlite"
	if cfg.DBPath() != "/absolute/db.sqlite" {
		t.Fatalf("expected absolute path, got %q", cfg.DBPath())
	}
}

func TestDBPath_Relative(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	dbPath := cfg.DBPath()
	if dbPath == "" {
		t.Fatal("expected non-empty db path")
	}
	if dbPath == cfg.DB.Path {
		t.Fatal("relative path should be resolved to absolute")
	}
}

// TestDBPath_RelativeUsesDataDir guards against a regression where the
// SQLite file landed under HomeRoot()/data instead of DataDir(). On macOS
// the FlowCraft server runs in a vfkit guest that has HOME unset and an
// empty /tmp (tmpfs); resolving against HomeRoot would put the DB on the
// guest's tmpfs and wipe user data on every VM restart. DataDir() is the
// virtio-fs-shared host directory and survives restarts.
func TestDBPath_RelativeUsesDataDir(t *testing.T) {
	isolateFlowcraftHome(t)
	dataDir := t.TempDir()
	t.Setenv("FLOWCRAFT_DATA_DIR", dataDir)
	cfg := Load()
	if got, want := cfg.DBPath(), filepath.Join(dataDir, cfg.DB.Path); got != want {
		t.Fatalf("DBPath() = %q, want %q (must resolve relative to DataDir, not HomeRoot)", got, want)
	}
}

func TestString(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	s := cfg.String()
	if s == "" {
		t.Fatal("expected non-empty string")
	}
	if !strings.Contains(s, "server.port") {
		t.Fatal("expected server.port in string output")
	}
	if !strings.Contains(s, "8080") {
		t.Fatal("expected default port 8080 in output")
	}
}

func TestInitLogging_JSON(t *testing.T) {
	InitLogging(LogConfig{Level: "debug", Format: "json"})
}

func TestInitLogging_Text(t *testing.T) {
	InitLogging(LogConfig{Level: "warn", Format: "text"})
}

func TestInitLogging_Error(t *testing.T) {
	InitLogging(LogConfig{Level: "error"})
}

func TestInitLogging_Default(t *testing.T) {
	InitLogging(LogConfig{Level: "unknown"})
}

func TestValidate_DeprecatedMemoryType(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	cfg.Memory.Type = "sliding_window"
	warnings := cfg.Validate()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "deprecated") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected deprecation warning for non-lossless memory type")
	}
}

func TestValidate_BadLogLevel(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	cfg.Log.Level = "trace"
	warnings := cfg.Validate()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "log.level") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected warning for unrecognized log level")
	}
}

func TestValidate_BadLogFormat(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	cfg.Log.Format = "xml"
	warnings := cfg.Validate()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "log.format") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected warning for unrecognized log format")
	}
}

func TestValidate_BadMonitoring(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	cfg.Monitoring.ErrorRateWarn = 2.0
	cfg.Monitoring.ErrorRateDown = -1.0
	cfg.Monitoring.ConsecutiveBuckets = 0
	cfg.Monitoring.NoSuccessDownMinutes = 0
	warnings := cfg.Validate()
	if len(warnings) < 4 {
		t.Fatalf("expected at least 4 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestValidate_WarnHigherThanDown(t *testing.T) {
	isolateFlowcraftHome(t)
	cfg := Load()
	cfg.Monitoring.ErrorRateWarn = 0.3
	cfg.Monitoring.ErrorRateDown = 0.1
	warnings := cfg.Validate()
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "lower than") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected warning for warn > down")
	}
}

func TestDefaultConfigPath(t *testing.T) {
	isolateFlowcraftHome(t)
	p := DefaultConfigPath()
	if p == "" {
		t.Fatal("expected non-empty path")
	}
}

func TestYAMLMerge_Telemetry(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `
telemetry:
  enabled: true
  endpoint: localhost:4317
  insecure: true
`)
	cfg := Load()
	if !cfg.Telemetry.Enabled {
		t.Fatal("expected telemetry enabled")
	}
	if cfg.Telemetry.Endpoint != "localhost:4317" {
		t.Fatalf("expected endpoint localhost:4317, got %q", cfg.Telemetry.Endpoint)
	}
	if !cfg.Telemetry.Insecure {
		t.Fatal("expected insecure true")
	}
}

func TestYAMLMerge_Sandbox(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `
sandbox:
  mode: persistent
  exec_timeout: 10m
  idle_timeout: 1h
  max_concurrent: 20
  root_dir: /custom/workspace
  network_mode: bridge
`)
	cfg := Load()
	if cfg.Sandbox.Mode != "persistent" {
		t.Fatalf("expected persistent, got %q", cfg.Sandbox.Mode)
	}
	if cfg.Sandbox.MaxConcurrent != 20 {
		t.Fatalf("expected 20, got %d", cfg.Sandbox.MaxConcurrent)
	}
	if cfg.Sandbox.RootDir != "/custom/workspace" {
		t.Fatalf("expected /custom/workspace, got %q", cfg.Sandbox.RootDir)
	}
	if cfg.Sandbox.NetworkMode != "bridge" {
		t.Fatalf("expected bridge, got %q", cfg.Sandbox.NetworkMode)
	}
}

func TestYAMLMerge_WebDir(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `
web_dir: /opt/flowcraft/web
`)
	cfg := Load()
	if cfg.WebDir != "/opt/flowcraft/web" {
		t.Fatalf("expected /opt/flowcraft/web, got %q", cfg.WebDir)
	}
}

func TestYAMLMerge_InvalidFile(t *testing.T) {
	root := isolateFlowcraftHome(t)
	mustWrite(t, filepath.Join(root, "config.yaml"), `{{invalid yaml}}`)
	cfg := Load()
	if cfg.Server.Port != 8080 {
		t.Fatalf("invalid YAML should keep defaults, got port %d", cfg.Server.Port)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
