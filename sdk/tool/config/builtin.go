package config

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/middleware"
)

// Built-in factory kinds, matching the sdk/tool/middleware constructors
// one-to-one.
const (
	KindRecover     = "recover"
	KindTelemetry   = "telemetry"
	KindConcurrency = "concurrency"
	KindTimeout     = "timeout"
	KindRateLimit   = "ratelimit"
	KindApproval    = "approval"
	KindAudit       = "audit"
	KindResultLimit = "resultlimit"
	KindRedact      = "redact"
)

func (b *Builder) registerBuiltins() {
	register := func(kind string, factory MiddlewareFactory) {
		if err := b.middlewares.Register(kind, factory); err != nil {
			panic(err)
		}
	}
	register(KindRecover, noSpecFactory(KindRecover,
		func() tool.Middleware { return middleware.Recover() }))
	register(KindTelemetry, noSpecFactory(KindTelemetry,
		func() tool.Middleware { return middleware.Telemetry() }))
	register(KindConcurrency, concurrencyFactory)
	register(KindTimeout, b.timeoutFactory)
	register(KindRateLimit, b.rateLimitFactory)
	register(KindApproval, b.approvalFactory)
	register(KindAudit, b.auditFactory)
	register(KindResultLimit, resultLimitFactory)
	register(KindRedact, redactFactory)
}

// noSpecFactory wraps a plain constructor, rejecting any spec: the kind
// has no knobs, so a spec in the document means the author confused
// kinds.
func noSpecFactory(kind string, make_ func() tool.Middleware) MiddlewareFactory {
	return func(_ context.Context, in sdkconfig.Input) (tool.Middleware, error) {
		if !isEmptySpec(in.Settings) {
			return nil, errdefs.Validation(fmt.Errorf(
				"kind %q takes no spec", kind))
		}
		return make_(), nil
	}
}

type concurrencySpec struct {
	Limit int `json:"limit"`
}

func concurrencyFactory(_ context.Context, in sdkconfig.Input) (tool.Middleware, error) {
	s, err := DecodeSpec[concurrencySpec](in.Settings)
	if err != nil {
		return nil, err
	}
	if s.Limit <= 0 {
		return nil, errdefs.Validation(fmt.Errorf(
			"concurrency spec: limit must be positive, got %d", s.Limit))
	}
	return middleware.Concurrency(s.Limit), nil
}

// Duration is a duration string such as "30s" or "2m". Unitless
// numbers are rejected on purpose: units belong in the config file.
type Duration time.Duration

// UnmarshalJSON rejects unitless numbers and parses Go duration strings.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\"")
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	*d = Duration(parsed)
	return nil
}

type timeoutSpec struct {
	Default Duration            `json:"default,omitempty"`
	PerTool map[string]Duration `json:"per_tool,omitempty"`
}

// timeoutFactory closes over the Builder's registry so the middleware
// can honour each tool's ToolMeta.SelfTimeout claim — the same catalog
// the Executor dispatches on. A per_tool entry in the spec still wins,
// keeping host policy authoritative over a tool's self-declaration.
func (b *Builder) timeoutFactory(_ context.Context, in sdkconfig.Input) (tool.Middleware, error) {
	s, err := DecodeSpec[timeoutSpec](in.Settings)
	if err != nil {
		return nil, err
	}
	catalog, err := catalogFrom(in)
	if err != nil {
		return nil, err
	}
	perTool := make(map[string]time.Duration, len(s.PerTool))
	for name, d := range s.PerTool {
		perTool[name] = time.Duration(d)
	}
	return middleware.TimeoutWithCatalog(catalog, time.Duration(s.Default), perTool), nil
}

// rateLimitFactory closes over the Builder's registry: the middleware
// resolves per-tool ToolMeta.RateLimit from the same catalog the
// Executor dispatches on.
func (b *Builder) rateLimitFactory(_ context.Context, in sdkconfig.Input) (tool.Middleware, error) {
	if !isEmptySpec(in.Settings) {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q takes no spec", KindRateLimit))
	}
	catalog, err := catalogFrom(in)
	if err != nil {
		return nil, err
	}
	return middleware.RateLimit(catalog), nil
}

type approvalSpec struct {
	Tools []string `json:"tools"`
}

func (b *Builder) approvalFactory(_ context.Context, in sdkconfig.Input) (tool.Middleware, error) {
	s, err := DecodeSpec[approvalSpec](in.Settings)
	if err != nil {
		return nil, err
	}
	if len(s.Tools) == 0 {
		return nil, errdefs.Validation(fmt.Errorf(
			"approval spec: tools must list at least one tool name"))
	}
	if b.deps.Approver == nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q requires an Approver in config.Deps", KindApproval))
	}
	return middleware.Approval(b.deps.Approver, s.Tools...), nil
}

func (b *Builder) auditFactory(_ context.Context, in sdkconfig.Input) (tool.Middleware, error) {
	if !isEmptySpec(in.Settings) {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q takes no spec", KindAudit))
	}
	if b.deps.AuditSink == nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q requires an AuditSink in config.Deps", KindAudit))
	}
	return middleware.Audit(b.deps.AuditSink), nil
}

type resultLimitSpec struct {
	MaxChars int    `json:"max_chars"`
	Marker   string `json:"marker,omitempty"`
}

func resultLimitFactory(_ context.Context, in sdkconfig.Input) (tool.Middleware, error) {
	s, err := DecodeSpec[resultLimitSpec](in.Settings)
	if err != nil {
		return nil, err
	}
	if s.MaxChars <= 0 {
		return nil, errdefs.Validation(fmt.Errorf(
			"resultlimit spec: max_chars must be positive, got %d", s.MaxChars))
	}
	return middleware.ResultLimiter(s.MaxChars,
		middleware.WithResultMarker(s.Marker)), nil
}

type redactSpec struct {
	Rules []redactRuleSpec `json:"rules"`
}

type redactRuleSpec struct {
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement,omitempty"`
}

func redactFactory(_ context.Context, in sdkconfig.Input) (tool.Middleware, error) {
	s, err := DecodeSpec[redactSpec](in.Settings)
	if err != nil {
		return nil, err
	}
	if len(s.Rules) == 0 {
		return nil, errdefs.Validation(fmt.Errorf(
			"redact spec: rules must list at least one pattern"))
	}
	rules := make([]middleware.RedactRule, 0, len(s.Rules))
	for i, rule := range s.Rules {
		if rule.Pattern == "" {
			return nil, errdefs.Validation(fmt.Errorf(
				"redact spec: rules[%d]: pattern is required", i))
		}
		compiled, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"redact spec: rules[%d]: invalid pattern %q: %w", i, rule.Pattern, err))
		}
		rules = append(rules, middleware.RedactRule{
			Pattern:     compiled,
			Replacement: rule.Replacement,
		})
	}
	return middleware.Redact(rules...), nil
}

func catalogFrom(in sdkconfig.Input) (tool.Catalog, error) {
	raw, ok := in.Dep(RegistryDep)
	if !ok {
		return nil, errdefs.Internalf(
			"tool config: middleware input is missing the %q dependency",
			RegistryDep)
	}
	catalog, ok := raw.(tool.Catalog)
	if !ok {
		return nil, errdefs.Internalf(
			"tool config: %q dependency has type %T, want tool.Catalog",
			RegistryDep, raw)
	}
	return catalog, nil
}
