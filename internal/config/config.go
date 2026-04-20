// Package config provides configuration loading for the FlowCraft server.
//
// Defaults are merged with ~/.flowcraft/config.yaml (see mergeYAML). The layout root
// is fixed at [HomeRoot] (~/.flowcraft); use config.yaml or CLI to change behavior.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Config is the top-level application configuration.
type Config struct {
	Server     ServerConfig
	Memory     MemoryConfig
	Auth       AuthConfig
	Log        LogConfig
	Sandbox    SandboxConfig
	Daemon     DaemonConfig
	Telemetry  TelemetryConfig
	DB         DBConfig
	Plugin     PluginConfig
	Monitoring MonitoringConfig

	Skills SkillsConfig

	WebDir        string // path to frontend build directory
	ConfigurePath string // directory for persistent configuration files
}

// SkillsConfig holds global per-skill configuration.
type SkillsConfig struct {
	Entries map[string]SkillEntryConfig `json:"entries,omitempty"`
}

// SkillEntryConfig configures a single installed skill.
type SkillEntryConfig struct {
	Enabled *bool             `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	APIKey  string            `json:"api_key,omitempty" yaml:"api_key,omitempty"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host                     string
	Port                     int
	RateLimitRPS             float64
	RateLimitBurst           int
	RateLimitBucketExpiry    int
	RateLimitCleanupInterval int
	MaxBodySize              int64
	CORSOrigins              []string
}

// MemoryConfig holds conversation memory settings.
// All memory is lossless by default; Type is deprecated and ignored.
type MemoryConfig struct {
	Type string // deprecated: ignored, always lossless
}

// AuthConfig holds authentication settings.
// JWT secret is managed in the DB (settings table), not in config.
type AuthConfig struct{}

// LogConfig holds logging settings.
type LogConfig struct {
	Level  string // debug, info, warn, error
	Format string // text, json

	// File controls the rotating server log sink. Always enabled so that
	// `flowcraft logs` has something to read on every platform; set
	// File.Path to "" to disable.
	File LogFileConfig
}

// LogFileConfig configures the rotating file log sink. Mirrors
// sdkx/telemetry/logfile.Config; resolved against ConfigurePath when Path
// is relative.
type LogFileConfig struct {
	Path       string // log file path; empty disables file sink
	MaxSizeMB  int    // rotate when file exceeds this size; 0 = sdk default
	MaxBackups int    // number of rotated files to keep; 0 = sdk default
	MaxAgeDays int    // delete rotated files older than this; 0 = sdk default
	Compress   bool   // gzip rotated files
}

// SandboxConfig holds sandbox execution environment settings.
type SandboxConfig struct {
	Mode          string // "ephemeral", "session" (default), "persistent"
	ExecTimeout   string
	IdleTimeout   string
	MaxConcurrent int
	RootDir       string
	NetworkMode   string // "none" (default), "bridge", "host"
}

// DaemonConfig holds daemon mode settings.
type DaemonConfig struct {
	Enabled      bool
	DefaultAgent string
}

// TelemetryConfig controls OpenTelemetry export.
type TelemetryConfig struct {
	Enabled  bool
	Endpoint string // OTLP endpoint, e.g. "localhost:4317"
	Insecure bool   // use insecure gRPC connection
}

// DBConfig controls the primary database.
type DBConfig struct {
	Path string // SQLite database file path (relative to ConfigurePath if not absolute)
}

// PluginConfig holds external plugin settings.
type PluginConfig struct {
	Dir            string `json:"dir"`             // plugin binary directory, default "plugins/"
	ConfigFile     string `json:"config_file"`     // plugins.json path (optional)
	HealthInterval int    `json:"health_interval"` // health check interval in seconds, default 10
	MaxFailures    int    `json:"max_failures"`    // max consecutive failures before restart, default 3
	MaxUploadSize  int64  `json:"max_upload_size"` // max plugin upload size in bytes, default 100MB
}

// MonitoringConfig holds monitoring page threshold settings.
type MonitoringConfig struct {
	ErrorRateWarn        float64
	ErrorRateDown        float64
	LatencyP95WarnMs     int64
	ConsecutiveBuckets   int
	NoSuccessDownMinutes int
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Memory: MemoryConfig{
			Type: "lossless",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
			File: LogFileConfig{
				Path:       "logs/server.log",
				MaxSizeMB:  100,
				MaxBackups: 7,
				MaxAgeDays: 30,
			},
		},
		Sandbox: SandboxConfig{
			Mode:          "session",
			ExecTimeout:   "5m",
			IdleTimeout:   "30m",
			MaxConcurrent: 10,
			NetworkMode:   "none",
		},
		DB: DBConfig{
			Path: "data/flowcraft.db",
		},
		Plugin: PluginConfig{
			MaxUploadSize: 100 << 20, // 100 MB
		},
		Monitoring: MonitoringConfig{
			ErrorRateWarn:        0.05,
			ErrorRateDown:        0.20,
			LatencyP95WarnMs:     3000,
			ConsecutiveBuckets:   3,
			NoSuccessDownMinutes: 2,
		},
		Skills: SkillsConfig{Entries: make(map[string]SkillEntryConfig)},
		WebDir: "web/dist",
	}
}

// Load creates configuration from defaults merged with ~/.flowcraft/config.yaml (if present).
func Load() *Config {
	cfg := Default()
	cfg.ConfigurePath = HomeRoot()
	mergeYAML(cfg, filepath.Join(cfg.ConfigurePath, "config.yaml"))
	return cfg
}

// Validate checks the configuration for potential issues and returns a list
// of warnings. It does not block startup — callers should log the warnings.
func (c *Config) Validate() []string {
	var warnings []string
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		warnings = append(warnings, fmt.Sprintf("server.port %d is out of valid range (1-65535)", c.Server.Port))
	}
	if c.Memory.Type != "" && c.Memory.Type != "lossless" {
		warnings = append(warnings, fmt.Sprintf("memory.type %q is deprecated; all memory is now lossless", c.Memory.Type))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		warnings = append(warnings, fmt.Sprintf("log.level %q is not recognized (expected: debug, info, warn, error)", c.Log.Level))
	}
	switch c.Log.Format {
	case "text", "json", "":
	default:
		warnings = append(warnings, fmt.Sprintf("log.format %q is not recognized (expected: text, json)", c.Log.Format))
	}
	if c.Log.File.MaxSizeMB < 0 {
		warnings = append(warnings, fmt.Sprintf("log.file.max_size_mb %d must be >= 0", c.Log.File.MaxSizeMB))
	}
	if c.Log.File.MaxBackups < 0 {
		warnings = append(warnings, fmt.Sprintf("log.file.max_backups %d must be >= 0", c.Log.File.MaxBackups))
	}
	if c.Log.File.MaxAgeDays < 0 {
		warnings = append(warnings, fmt.Sprintf("log.file.max_age_days %d must be >= 0", c.Log.File.MaxAgeDays))
	}
	if c.Monitoring.ErrorRateWarn < 0 || c.Monitoring.ErrorRateWarn > 1 {
		warnings = append(warnings, fmt.Sprintf("monitoring.error_rate_warn %.4f is out of range [0,1]", c.Monitoring.ErrorRateWarn))
	}
	if c.Monitoring.ErrorRateDown < 0 || c.Monitoring.ErrorRateDown > 1 {
		warnings = append(warnings, fmt.Sprintf("monitoring.error_rate_down %.4f is out of range [0,1]", c.Monitoring.ErrorRateDown))
	}
	if c.Monitoring.ErrorRateDown < c.Monitoring.ErrorRateWarn {
		warnings = append(warnings, "monitoring.error_rate_down is lower than monitoring.error_rate_warn")
	}
	if c.Monitoring.ConsecutiveBuckets <= 0 {
		warnings = append(warnings, "monitoring.consecutive_buckets must be > 0")
	}
	if c.Monitoring.NoSuccessDownMinutes <= 0 {
		warnings = append(warnings, "monitoring.no_success_down_minutes must be > 0")
	}
	return warnings
}

// Address returns the listen address string "host:port".
func (c *Config) Address() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// DBPath returns the absolute database path, resolved relative to ConfigurePath.
func (c *Config) DBPath() string {
	if filepath.IsAbs(c.DB.Path) {
		return c.DB.Path
	}
	return filepath.Join(c.ConfigurePath, c.DB.Path)
}

// LogFilePath returns the absolute server log file path, resolved relative
// to DataDir (not ConfigurePath). Returns "" if the file sink is disabled.
//
// The file lives under DataDir so that on macOS — where the server runs in
// a vfkit guest — DataDir is virtio-fs-shared with the host and `flowcraft
// logs` on the host can read the same file the guest server is writing
// to.
func (c *Config) LogFilePath() string {
	if c.Log.File.Path == "" {
		return ""
	}
	if filepath.IsAbs(c.Log.File.Path) {
		return c.Log.File.Path
	}
	return filepath.Join(DataDir(), c.Log.File.Path)
}

// String returns a summary of the config (with secrets masked).
func (c *Config) String() string {
	var b strings.Builder
	b.WriteString("FlowCraft Configuration:\n")
	fmt.Fprintf(&b, "  server.host:      %s\n", c.Server.Host)
	fmt.Fprintf(&b, "  server.port:      %d\n", c.Server.Port)
	fmt.Fprintf(&b, "  memory:           lossless\n")
	fmt.Fprintf(&b, "  auth:             jwt (secret in DB)\n")
	fmt.Fprintf(&b, "  log.level:        %s\n", c.Log.Level)
	fmt.Fprintf(&b, "  log.format:       %s\n", c.Log.Format)
	if p := c.LogFilePath(); p != "" {
		fmt.Fprintf(&b, "  log.file.path:    %s\n", p)
	} else {
		fmt.Fprintf(&b, "  log.file.path:    (disabled)\n")
	}
	fmt.Fprintf(&b, "  configure_path:   %s\n", c.ConfigurePath)
	fmt.Fprintf(&b, "  sandbox.mode:     %s\n", c.Sandbox.Mode)
	fmt.Fprintf(&b, "  db.path:          %s\n", c.DB.Path)
	fmt.Fprintf(&b, "  telemetry:        %v\n", c.Telemetry.Enabled)
	fmt.Fprintf(&b, "  monitoring.warn:  %.2f\n", c.Monitoring.ErrorRateWarn)
	fmt.Fprintf(&b, "  monitoring.down:  %.2f\n", c.Monitoring.ErrorRateDown)
	return b.String()
}

func maskSecret(s string) string {
	if s == "" {
		return "(not set)"
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}

// InitLogging configures the global slog logger based on LogConfig.
func InitLogging(cfg LogConfig) {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))
}
