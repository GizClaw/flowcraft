module github.com/GizClaw/flowcraft/tool

go 1.26.0

// The core pin is a commit on main, not a tag, so a plain `go mod tidy` cannot resolve it yet:
// `make release-preflight` is what tidies this block against the batch it ships with. Everything
// below is what the packages import, and nothing else — the exec tests drive a starter of their own
// (exec/fake_test.go) rather than depending on a backend module.
require (
	github.com/GizClaw/flowcraft/core v0.11.3-0.20261124053012-dbfa04813d3a
	github.com/goccy/go-json v0.12.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/metric v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	golang.org/x/sys v0.48.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.47.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	sigs.k8s.io/yaml v1.6.0 // indirect
)

