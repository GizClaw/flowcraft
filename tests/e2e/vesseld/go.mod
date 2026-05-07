module github.com/GizClaw/flowcraft/tests/e2e/vesseld

go 1.25.0

require github.com/GizClaw/flowcraft/cmd/vesseld v0.0.0

require (
	github.com/GizClaw/flowcraft/sdk v0.2.7 // indirect
	github.com/GizClaw/flowcraft/sdkx v0.2.5 // indirect
	github.com/GizClaw/flowcraft/vessel v0.1.0-rc.2 // indirect
	github.com/anthropics/anthropic-sdk-go v1.26.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dlclark/regexp2 v1.11.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/openai/openai-go v1.12.0 // indirect
	github.com/pkoukk/tiktoken-go v0.1.8 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/volcengine/volc-sdk-golang v1.0.23 // indirect
	github.com/volcengine/volcengine-go-sdk v1.2.14 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.40.0 // indirect
	go.opentelemetry.io/otel/log v0.16.0 // indirect
	go.opentelemetry.io/otel/metric v1.40.0 // indirect
	go.opentelemetry.io/otel/sdk v1.40.0 // indirect
	go.opentelemetry.io/otel/sdk/log v0.16.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.40.0 // indirect
	go.opentelemetry.io/otel/trace v1.40.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/GizClaw/flowcraft/cmd/vesseld => ../../../cmd/vesseld
	github.com/GizClaw/flowcraft/sdk => ../../../sdk
	github.com/GizClaw/flowcraft/sdkx => ../../../sdkx
	github.com/GizClaw/flowcraft/vessel => ../../../vessel
)
