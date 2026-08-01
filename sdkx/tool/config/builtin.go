package config

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/middleware"
	yamlv3 "gopkg.in/yaml.v3"
)

// Built-in factory kinds, matching the sdk/tool/middleware
// constructors one-to-one.
const (
	KindRecover     = "recover"
	KindTelemetry   = "telemetry"
	KindConcurrency = "concurrency"
	KindTimeout     = "timeout"
	KindRateLimit   = "ratelimit"
	KindApproval    = "approval"
	KindAudit       = "audit"
)

func (b *Builder) registerBuiltins() {
	b.factories[KindRecover] = noSpecFactory(KindRecover,
		func() tool.Middleware { return middleware.Recover() })
	b.factories[KindTelemetry] = noSpecFactory(KindTelemetry,
		func() tool.Middleware { return middleware.Telemetry() })
	b.factories[KindConcurrency] = concurrencyFactory
	b.factories[KindTimeout] = b.timeoutFactory
	b.factories[KindRateLimit] = b.rateLimitFactory
	b.factories[KindApproval] = b.approvalFactory
	b.factories[KindAudit] = b.auditFactory
}

// noSpecFactory wraps a plain constructor, rejecting any spec: the
// kind has no knobs, so a spec in YAML means the author confused
// kinds.
func noSpecFactory(kind string, make_ func() tool.Middleware) MiddlewareFactory {
	return func(_ context.Context, spec *yamlv3.Node) (tool.Middleware, error) {
		if !isEmptySpec(spec) {
			return nil, errdefs.Validation(fmt.Errorf(
				"kind %q takes no spec", kind))
		}
		return make_(), nil
	}
}

type concurrencySpec struct {
	Limit int `yaml:"limit"`
}

func concurrencyFactory(_ context.Context, spec *yamlv3.Node) (tool.Middleware, error) {
	s, err := DecodeSpec[concurrencySpec](spec)
	if err != nil {
		return nil, err
	}
	if s.Limit <= 0 {
		return nil, errdefs.Validation(fmt.Errorf(
			"concurrency spec: limit must be positive, got %d", s.Limit))
	}
	return middleware.Concurrency(s.Limit), nil
}

// Duration is a YAML duration string such as "30s" or "2m". Unitless
// numbers are rejected on purpose: units belong in the config file.
type Duration time.Duration

// UnmarshalYAML rejects unitless numbers and parses Go duration strings.
func (d *Duration) UnmarshalYAML(node *yamlv3.Node) error {
	if node.Kind != yamlv3.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("duration must be a string like \"30s\"")
	}
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(v)
	return nil
}

type timeoutSpec struct {
	Default Duration            `yaml:"default,omitempty"`
	PerTool map[string]Duration `yaml:"per_tool,omitempty"`
}

// timeoutFactory closes over the Builder's registry so the middleware
// can honour each tool's ToolMeta.SelfTimeout claim — the same catalog
// the Executor dispatches on. A per_tool entry in the spec still wins,
// keeping host policy authoritative over a tool's self-declaration.
func (b *Builder) timeoutFactory(_ context.Context, spec *yamlv3.Node) (tool.Middleware, error) {
	s, err := DecodeSpec[timeoutSpec](spec)
	if err != nil {
		return nil, err
	}
	perTool := make(map[string]time.Duration, len(s.PerTool))
	for name, d := range s.PerTool {
		perTool[name] = time.Duration(d)
	}
	return middleware.TimeoutWithCatalog(b.registry, time.Duration(s.Default), perTool), nil
}

// rateLimitFactory closes over the Builder's registry: the
// middleware resolves per-tool ToolMeta.RateLimit from the same
// catalog the Executor dispatches on.
func (b *Builder) rateLimitFactory(_ context.Context, spec *yamlv3.Node) (tool.Middleware, error) {
	if !isEmptySpec(spec) {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q takes no spec", KindRateLimit))
	}
	return middleware.RateLimit(b.registry), nil
}

type approvalSpec struct {
	Tools []string `yaml:"tools"`
}

func (b *Builder) approvalFactory(_ context.Context, spec *yamlv3.Node) (tool.Middleware, error) {
	s, err := DecodeSpec[approvalSpec](spec)
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

func (b *Builder) auditFactory(_ context.Context, spec *yamlv3.Node) (tool.Middleware, error) {
	if !isEmptySpec(spec) {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q takes no spec", KindAudit))
	}
	if b.deps.AuditSink == nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q requires an AuditSink in config.Deps", KindAudit))
	}
	return middleware.Audit(b.deps.AuditSink), nil
}
