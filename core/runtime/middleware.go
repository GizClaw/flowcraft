package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/middleware"

	"go.opentelemetry.io/otel/attribute"
)

// Built-in middleware names. A deployment lists these in tools.middleware, in
// the order they wrap the chain: first written is outermost.
const (
	MiddlewareTelemetry   = "telemetry"
	MiddlewareRecover     = "recover"
	MiddlewareResultLimit = "result_limit"
	MiddlewareConcurrency = "concurrency"
	MiddlewareRedact      = "redact"
)

// MiddlewareOption registers a named middleware the deployment may list.
//
// Middleware that needs a Go value — an approver, an audit sink, a rate-limit
// catalog — cannot come from YAML, so the host supplies it here, exactly the way
// drivers and selectors arrive.
type MiddlewareOption struct {
	Name  string
	Build func(settings json.RawMessage) (tool.Middleware, error)
}

// WithMiddleware registers middleware names available to tools.middleware.
func WithMiddleware(entries ...MiddlewareOption) Option {
	return func(o *options) { o.middleware = append(o.middleware, entries...) }
}

func middlewareTable(registered []MiddlewareOption) (map[string]func(json.RawMessage) (tool.Middleware, error), error) {
	table := map[string]func(json.RawMessage) (tool.Middleware, error){
		MiddlewareTelemetry: func(json.RawMessage) (tool.Middleware, error) {
			return middleware.Telemetry(), nil
		},
		MiddlewareRecover: func(json.RawMessage) (tool.Middleware, error) {
			return middleware.Recover(), nil
		},
		MiddlewareResultLimit: func(settings json.RawMessage) (tool.Middleware, error) {
			max, err := intSetting(settings, "max_bytes")
			if err != nil {
				return nil, err
			}
			return middleware.ResultLimiter(max)
		},
		MiddlewareConcurrency: func(settings json.RawMessage) (tool.Middleware, error) {
			limit, err := intSetting(settings, "limit")
			if err != nil {
				return nil, err
			}
			return middleware.Concurrency(limit)
		},
		MiddlewareRedact: func(settings json.RawMessage) (tool.Middleware, error) {
			patterns, err := stringsSetting(settings, "patterns")
			if err != nil {
				return nil, err
			}
			if len(patterns) == 0 {
				return nil, errdefs.Validationf("redact: settings.patterns is required")
			}
			return middleware.RedactPatterns(patterns...), nil
		},
	}
	for _, option := range registered {
		if option.Name == "" {
			return nil, errorf("runtime: a middleware has no name")
		}
		if option.Build == nil {
			return nil, errorf("runtime: middleware %q has no constructor", option.Name)
		}
		if _, exists := table[option.Name]; exists {
			return nil, errorf("runtime: middleware %q is registered twice", option.Name)
		}
		table[option.Name] = option.Build
	}
	return table, nil
}

// middlewareChain builds the dispatch chain from tools.middleware.
//
// The order is the written order, outermost first: the first entry sees the call
// before any other, which is what makes "recover outermost" expressible.
//
// Recover cannot be disabled. A panicking tool that reaches the runtime takes
// the process with it, so that one is a floor rather than a deployment choice:
// when the list omits it, it is prepended and the substitution is logged.
func (r *Runtime) middlewareChain(ctx context.Context, table map[string]func(json.RawMessage) (tool.Middleware, error)) ([]tool.Middleware, error) {
	entries := r.cfg.Tools.Middleware
	if len(entries) == 0 {
		entries = []MiddlewareConfig{{Name: MiddlewareRecover}, {Name: MiddlewareTelemetry}}
	}
	chain := make([]tool.Middleware, 0, len(entries)+1)
	recovered := false
	for i, entry := range entries {
		build, known := table[entry.Name]
		if !known {
			return nil, errdefs.Validationf(
				"tools.middleware[%d]: unknown middleware %q; offered: %s",
				i, entry.Name, offeredMiddleware(table))
		}
		built, err := build(entry.Settings)
		if err != nil {
			return nil, errdefs.Validationf("tools.middleware[%d] (%s): %w", i, entry.Name, err)
		}
		if entry.Name == MiddlewareRecover {
			recovered = true
		}
		chain = append(chain, built)
	}
	if !recovered {
		telemetry.Warn(ctx, "tools.middleware omits recover; prepending it",
			attribute.String("reason", "a panicking tool must not stop the runtime"))
		chain = append([]tool.Middleware{middleware.Recover()}, chain...)
	}
	return chain, nil
}

// settingsObject decodes the middleware settings object, so each entry reads the
// keys it declares instead of a shared untyped map.
func settingsObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errdefs.Validationf("settings: %w", err)
	}
	return fields, nil
}

func intSetting(raw json.RawMessage, key string) (int, error) {
	fields, err := settingsObject(raw)
	if err != nil {
		return 0, err
	}
	value, found := fields[key]
	if !found {
		return 0, errdefs.Validationf("settings.%s is required", key)
	}
	var out int
	if err := json.Unmarshal(value, &out); err != nil {
		return 0, errdefs.Validationf("settings.%s: want an integer: %w", key, err)
	}
	return out, nil
}

func stringsSetting(raw json.RawMessage, key string) ([]string, error) {
	fields, err := settingsObject(raw)
	if err != nil {
		return nil, err
	}
	value, found := fields[key]
	if !found {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(value, &out); err != nil {
		return nil, errdefs.Validationf("settings.%s: want a list of strings: %w", key, err)
	}
	return out, nil
}

func offeredMiddleware(table map[string]func(json.RawMessage) (tool.Middleware, error)) string {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
