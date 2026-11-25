module github.com/GizClaw/flowcraft/test

go 1.26.0

require (
	github.com/GizClaw/flowcraft/agent v0.0.0
	github.com/GizClaw/flowcraft/core v0.11.3-0.20261124053012-dbfa04813d3a
	github.com/GizClaw/flowcraft/sandbox v0.0.0
	github.com/GizClaw/flowcraft/tool v0.0.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/creack/pty v1.1.24 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/expr-lang/expr v1.17.8 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/goccy/go-json v0.12.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/xid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/sys v0.48.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.59.0 // indirect
	sigs.k8s.io/yaml v1.6.0 // indirect
)

// One module, several packages: the assembler in internal/stack is what e2e, fault and
// perf share, and a package cannot be shared across module boundaries. The modules this
// test assembles are siblings in this workspace and are not all tagged yet, so the
// requirements are local paths rather than versions. They become versions — and the
// replaces disappear — at the release that tags them, exactly as examples/forge's did.
replace (
	github.com/GizClaw/flowcraft/agent => ../agent
	github.com/GizClaw/flowcraft/core => ../core
	github.com/GizClaw/flowcraft/sandbox => ../sandbox
	github.com/GizClaw/flowcraft/tool => ../tool
)
