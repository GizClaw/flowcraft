module github.com/GizClaw/flowcraft/tests/e2e/vesseld

go 1.25.0

require github.com/GizClaw/flowcraft/cmd/vesseld v0.0.0

require (
	github.com/GizClaw/flowcraft/sdk v0.2.7 // indirect
	github.com/GizClaw/flowcraft/sdkx v0.2.5 // indirect
	github.com/GizClaw/flowcraft/vessel v0.1.0-rc.2 // indirect
	github.com/anthropics/anthropic-sdk-go v1.26.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dlclark/regexp2 v1.11.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.27.7 // indirect
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
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp v0.16.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.40.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.40.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.40.0 // indirect
	go.opentelemetry.io/otel/log v0.16.0 // indirect
	go.opentelemetry.io/otel/metric v1.40.0 // indirect
	go.opentelemetry.io/otel/sdk v1.40.0 // indirect
	go.opentelemetry.io/otel/sdk/log v0.16.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.40.0 // indirect
	go.opentelemetry.io/otel/trace v1.40.0 // indirect
	go.opentelemetry.io/proto/otlp v1.9.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.35.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260427160629-7cedc36a6bc4 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260427160629-7cedc36a6bc4 // indirect
	google.golang.org/grpc v1.80.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Only replace modules that have no published version we can pin
// against. sdk / sdkx / vessel ARE published so they stay
// version-pinned — this isolates the e2e suite from in-flight
// library PRs and keeps a downstream PR's CI from breaking just
// because it bumps an indirect dep here.
//
// cmd/vesseld is the lone exception: it is intentionally never
// tagged as a Go module (it ships as a binary release artifact —
// see .github/workflows/auto-tag.yml), so the local-tree replace
// is mandatory.
replace github.com/GizClaw/flowcraft/cmd/vesseld => ../../../cmd/vesseld

// TEMPORARY (PR #96 — feat/engine-lifecycle-contracts): this PR
// introduces new sdk + vessel APIs (engine.WithCapabilities,
// vessel.Captain.Resume, …) that cmd/vesseld immediately consumes.
// Because cmd/vesseld is replace-pinned to the local tree but
// sdk + vessel are version-pinned, the e2e module would otherwise
// fail to build against the unreleased symbols.
//
// These two replaces relax the isolation policy for ONE PR; they
// MUST be removed in a follow-up after sdk + vessel auto-tag
// publishes the new versions and the `require` lines above are
// bumped accordingly. Tracking removal: TODO(post-tag).
replace github.com/GizClaw/flowcraft/sdk => ../../../sdk

replace github.com/GizClaw/flowcraft/vessel => ../../../vessel
