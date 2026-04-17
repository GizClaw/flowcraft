package bootstrap

import (
	"io/fs"

	"github.com/GizClaw/flowcraft/internal/api"
	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/gateway"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/internal/realm"
	"github.com/GizClaw/flowcraft/web"
)

// wireHTTP constructs the API server from the assembled Platform, gateway,
// config, and plugin directory path.
func wireHTTP(cfg *config.Config, plat *platform.Platform, gw *gateway.Gateway, pluginDir string) *api.Server {
	webFS, _ := fs.Sub(web.Dist, "dist")

	deps := api.ServerDeps{
		Store:        plat.Store,
		RuntimeMgr:   plat.Realms.(*realm.SingleRealmProvider),
		Compiler:     plat.Compiler,
		SchemaReg:    plat.SchemaReg,
		TemplateReg:  plat.TemplateReg,
		PluginReg:    plat.PluginReg,
		Knowledge:    plat.Knowledge,
		VersionStore: plat.VersionStore,
		LTStore:      plat.LTStore,
		EventBus:     plat.EventBus,
		LLMResolver:  plat.LLMResolver,
		SkillStore:   plat.SkillStore,
		ToolRegistry: plat.ToolRegistry,
		PluginDir:    pluginDir,
		Gateway:      gw,
		Monitoring: api.MonitoringConfig{
			ErrorRateWarn:        cfg.Monitoring.ErrorRateWarn,
			ErrorRateDown:        cfg.Monitoring.ErrorRateDown,
			LatencyP95WarnMs:     cfg.Monitoring.LatencyP95WarnMs,
			ConsecutiveBuckets:   cfg.Monitoring.ConsecutiveBuckets,
			NoSuccessDownMinutes: cfg.Monitoring.NoSuccessDownMinutes,
		},
	}

	return api.NewServer(api.ServerConfig{
		Host:           cfg.Server.Host,
		Port:           cfg.Server.Port,
		APIKey:         cfg.Auth.APIKey,
		RateLimitRPS:   cfg.Server.RateLimitRPS,
		RateLimitBurst: cfg.Server.RateLimitBurst,
		MaxUploadSize:  cfg.Plugin.MaxUploadSize,
		WebFS:          webFS,
	}, deps)
}
