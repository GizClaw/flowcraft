package bootstrap

import (
	"io/fs"

	"github.com/GizClaw/flowcraft/internal/api"
	auditcmd "github.com/GizClaw/flowcraft/internal/commands/audit"
	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/gateway"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/internal/policy"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
	senderwebhook "github.com/GizClaw/flowcraft/internal/senders/webhook"
	"github.com/GizClaw/flowcraft/web"
)

// wireHTTP constructs the API server from the assembled Platform, gateway,
// JWT config, and server settings.
func wireHTTP(
	cfg *config.Config,
	plat *platform.Platform,
	gw *gateway.Gateway,
	jwtCfg *api.JWTConfig,
	pluginDir string,
	eventLog *eventlog.SQLiteLog,
	auditCmds *auditcmd.Commands,
	pol policy.Policy,
	projMgr *projection.Manager,
	webhookSender *senderwebhook.WebhookOutboundSender,
	chatRead api.ChatReadModel,
) *api.Server {
	webFS, _ := fs.Sub(web.Dist, "dist")

	deps := api.ServerDeps{
		Platform:           plat,
		Gateway:            gw,
		PluginDir:          pluginDir,
		EventLog:           eventLog,
		AuditCmds:          auditCmds,
		Policy:             pol,
		ProjectionStatus:   newProjectionStatusAdapter(projMgr),
		ProjectionReplayer: newProjectionReplayAdapter(projMgr),
		WebhookReplayer:    newWebhookReplayAdapter(webhookSender),
		ChatRead:           chatRead,
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
		RateLimitRPS:   cfg.Server.RateLimitRPS,
		RateLimitBurst: cfg.Server.RateLimitBurst,
		MaxUploadSize:  cfg.Plugin.MaxUploadSize,
		WebFS:          webFS,
	}, deps, jwtCfg)
}
