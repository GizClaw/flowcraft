package bootstrap

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/gateway"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/realm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

func initGateway(ctx context.Context, runtimeMgr *realm.SingleRealmProvider, appStore model.Store) *gateway.Gateway {
	router := gateway.NewChannelRouter(appStore)
	if err := router.Reload(ctx); err != nil {
		telemetry.Warn(ctx, "gateway: initial channel reload failed",
			otellog.String("error", err.Error()))
	}
	return gateway.NewGateway(appStore, router, gateway.WithRealmProvider(runtimeMgr))
}
