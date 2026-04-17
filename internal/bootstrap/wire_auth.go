package bootstrap

import (
	"context"
	"encoding/base64"

	"github.com/GizClaw/flowcraft/internal/api"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

const settingKeyJWTSecret = "jwt_secret"

// wireAuth initializes the JWT configuration from the DB. If no secret exists
// yet, a new one is generated and persisted so it survives process restarts.
func wireAuth(ctx context.Context, store model.Store) (*api.JWTConfig, error) {
	secret, err := store.GetSetting(ctx, settingKeyJWTSecret)
	if err != nil || secret == "" {
		generated, genErr := api.GenerateSecret()
		if genErr != nil {
			return nil, genErr
		}
		if err := store.SetSetting(ctx, settingKeyJWTSecret, generated); err != nil {
			return nil, err
		}
		secret = generated
		telemetry.Info(ctx, "bootstrap: generated new JWT secret",
			otellog.String("key", settingKeyJWTSecret))
	}

	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil {
		raw = []byte(secret)
	}
	return &api.JWTConfig{Secret: raw}, nil
}
