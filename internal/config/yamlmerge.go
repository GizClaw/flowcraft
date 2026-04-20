package config

import (
	"context"
	"os"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"gopkg.in/yaml.v3"

	otellog "go.opentelemetry.io/otel/log"
)

// yamlFile mirrors the subset of config.yaml we merge into Config.
type yamlFile struct {
	Server *struct {
		Host                     string   `yaml:"host"`
		Port                     *int     `yaml:"port"`
		RateLimitRPS             *float64 `yaml:"rate_limit_rps"`
		RateLimitBurst           *int     `yaml:"rate_limit_burst"`
		RateLimitBucketExpiry    *int     `yaml:"rate_limit_bucket_expiry"`
		RateLimitCleanupInterval *int     `yaml:"rate_limit_cleanup_interval"`
		MaxBodySize              *int64   `yaml:"max_body_size"`
		CORSOrigins              []string `yaml:"cors_origins"`
	} `yaml:"server"`
	Log *struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
	} `yaml:"log"`
	Memory *struct {
		Type string `yaml:"type"`
	} `yaml:"memory"`
	Auth *struct {
		APIKey string `yaml:"api_key"`
	} `yaml:"auth"`
	DB *struct {
		Path string `yaml:"path"`
	} `yaml:"db"`
	Sandbox *struct {
		Mode          string `yaml:"mode"`
		ExecTimeout   string `yaml:"exec_timeout"`
		IdleTimeout   string `yaml:"idle_timeout"`
		MaxConcurrent *int   `yaml:"max_concurrent"`
		RootDir       string `yaml:"root_dir"`
		NetworkMode   string `yaml:"network_mode"`
	} `yaml:"sandbox"`
	Plugin *struct {
		Dir            string `yaml:"dir"`
		ConfigFile     string `yaml:"config_file"`
		HealthInterval *int   `yaml:"health_interval"`
		MaxFailures    *int   `yaml:"max_failures"`
		MaxUploadSize  *int64 `yaml:"max_upload_size"`
	} `yaml:"plugin"`
	Skills *struct {
		Entries map[string]SkillEntryConfig `yaml:"entries"`
	} `yaml:"skills"`
	Monitoring *struct {
		ErrorRateWarn        *float64 `yaml:"error_rate_warn"`
		ErrorRateDown        *float64 `yaml:"error_rate_down"`
		LatencyP95WarnMs     *int64   `yaml:"p95_warn_ms"`
		ConsecutiveBuckets   *int     `yaml:"consecutive_buckets"`
		NoSuccessDownMinutes *int     `yaml:"no_success_down_minutes"`
	} `yaml:"monitoring"`
	Telemetry *struct {
		Enabled  *bool  `yaml:"enabled"`
		Endpoint string `yaml:"endpoint"`
		Insecure *bool  `yaml:"insecure"`
	} `yaml:"telemetry"`
	WebDir string `yaml:"web_dir"`
}

func mergeYAML(cfg *Config, configPath string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		telemetryWarnYAML(configPath, err)
		return
	}
	var y yamlFile
	if err := yaml.Unmarshal(data, &y); err != nil {
		telemetryWarnYAML(configPath, err)
		return
	}
	if y.Server != nil {
		if y.Server.Host != "" {
			cfg.Server.Host = y.Server.Host
		}
		if y.Server.Port != nil {
			cfg.Server.Port = *y.Server.Port
		}
		if y.Server.RateLimitRPS != nil {
			cfg.Server.RateLimitRPS = *y.Server.RateLimitRPS
		}
		if y.Server.RateLimitBurst != nil {
			cfg.Server.RateLimitBurst = *y.Server.RateLimitBurst
		}
		if y.Server.RateLimitBucketExpiry != nil {
			cfg.Server.RateLimitBucketExpiry = *y.Server.RateLimitBucketExpiry
		}
		if y.Server.RateLimitCleanupInterval != nil {
			cfg.Server.RateLimitCleanupInterval = *y.Server.RateLimitCleanupInterval
		}
		if y.Server.MaxBodySize != nil {
			cfg.Server.MaxBodySize = *y.Server.MaxBodySize
		}
		if len(y.Server.CORSOrigins) > 0 {
			cfg.Server.CORSOrigins = y.Server.CORSOrigins
		}
	}
	if y.Log != nil {
		if y.Log.Level != "" {
			cfg.Log.Level = y.Log.Level
		}
		if y.Log.Format != "" {
			cfg.Log.Format = y.Log.Format
		}
	}
	if y.Memory != nil && y.Memory.Type != "" {
		cfg.Memory.Type = y.Memory.Type
	}
	// Auth is now JWT-based; secret is managed in the DB, not in config.yaml.
	if y.DB != nil && y.DB.Path != "" {
		cfg.DB.Path = y.DB.Path
	}
	if y.Sandbox != nil {
		if y.Sandbox.Mode != "" {
			cfg.Sandbox.Mode = y.Sandbox.Mode
		}
		if y.Sandbox.ExecTimeout != "" {
			cfg.Sandbox.ExecTimeout = y.Sandbox.ExecTimeout
		}
		if y.Sandbox.IdleTimeout != "" {
			cfg.Sandbox.IdleTimeout = y.Sandbox.IdleTimeout
		}
		if y.Sandbox.MaxConcurrent != nil {
			cfg.Sandbox.MaxConcurrent = *y.Sandbox.MaxConcurrent
		}
		if y.Sandbox.RootDir != "" {
			cfg.Sandbox.RootDir = y.Sandbox.RootDir
		}
		if y.Sandbox.NetworkMode != "" {
			cfg.Sandbox.NetworkMode = y.Sandbox.NetworkMode
		}
	}
	if y.Plugin != nil {
		if y.Plugin.Dir != "" {
			cfg.Plugin.Dir = y.Plugin.Dir
		}
		if y.Plugin.ConfigFile != "" {
			cfg.Plugin.ConfigFile = y.Plugin.ConfigFile
		}
		if y.Plugin.HealthInterval != nil {
			cfg.Plugin.HealthInterval = *y.Plugin.HealthInterval
		}
		if y.Plugin.MaxFailures != nil {
			cfg.Plugin.MaxFailures = *y.Plugin.MaxFailures
		}
		if y.Plugin.MaxUploadSize != nil {
			cfg.Plugin.MaxUploadSize = *y.Plugin.MaxUploadSize
		}
	}
	if y.Skills != nil && len(y.Skills.Entries) > 0 {
		cfg.Skills.Entries = y.Skills.Entries
	}
	if y.Monitoring != nil {
		if y.Monitoring.ErrorRateWarn != nil {
			cfg.Monitoring.ErrorRateWarn = *y.Monitoring.ErrorRateWarn
		}
		if y.Monitoring.ErrorRateDown != nil {
			cfg.Monitoring.ErrorRateDown = *y.Monitoring.ErrorRateDown
		}
		if y.Monitoring.LatencyP95WarnMs != nil {
			cfg.Monitoring.LatencyP95WarnMs = *y.Monitoring.LatencyP95WarnMs
		}
		if y.Monitoring.ConsecutiveBuckets != nil {
			cfg.Monitoring.ConsecutiveBuckets = *y.Monitoring.ConsecutiveBuckets
		}
		if y.Monitoring.NoSuccessDownMinutes != nil {
			cfg.Monitoring.NoSuccessDownMinutes = *y.Monitoring.NoSuccessDownMinutes
		}
	}
	if y.Telemetry != nil {
		if y.Telemetry.Enabled != nil {
			cfg.Telemetry.Enabled = *y.Telemetry.Enabled
		}
		if y.Telemetry.Endpoint != "" {
			cfg.Telemetry.Endpoint = y.Telemetry.Endpoint
		}
		if y.Telemetry.Insecure != nil {
			cfg.Telemetry.Insecure = *y.Telemetry.Insecure
		}
	}
	if y.WebDir != "" {
		cfg.WebDir = y.WebDir
	}
}

func telemetryWarnYAML(path string, err error) {
	telemetry.Warn(context.Background(), "config: failed to read or parse yaml",
		otellog.String("path", path),
		otellog.String("error", err.Error()))
}

// DefaultConfigPath returns the path to config.yaml under the FlowCraft home.
func DefaultConfigPath() string {
	return ConfigFile()
}
