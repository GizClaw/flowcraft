package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/middleware"
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
	return func(_ context.Context, spec json.RawMessage) (tool.Middleware, error) {
		if len(spec) > 0 && string(spec) != "null" && string(spec) != "{}" {
			return nil, errdefs.Validation(fmt.Errorf(
				"kind %q takes no spec", kind))
		}
		return make_(), nil
	}
}

type concurrencySpec struct {
	Limit int `json:"limit"`
}

func concurrencyFactory(_ context.Context, spec json.RawMessage) (tool.Middleware, error) {
	var s concurrencySpec
	if err := decodeSpec(spec, &s); err != nil {
		return nil, err
	}
	if s.Limit <= 0 {
		return nil, errdefs.Validation(fmt.Errorf(
			"concurrency spec: limit must be positive, got %d", s.Limit))
	}
	return middleware.Concurrency(s.Limit), nil
}

// Duration decodes a Go duration string ("30s", "2m") from JSON.
// Numbers are rejected on purpose: units belong in the config file.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\", got %s", data)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
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
func (b *Builder) timeoutFactory(_ context.Context, spec json.RawMessage) (tool.Middleware, error) {
	var s timeoutSpec
	if err := decodeSpec(spec, &s); err != nil {
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
func (b *Builder) rateLimitFactory(_ context.Context, spec json.RawMessage) (tool.Middleware, error) {
	if len(spec) > 0 && string(spec) != "null" && string(spec) != "{}" {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q takes no spec", KindRateLimit))
	}
	return middleware.RateLimit(b.registry), nil
}

type approvalSpec struct {
	Tools []string `json:"tools"`
}

func (b *Builder) approvalFactory(_ context.Context, spec json.RawMessage) (tool.Middleware, error) {
	var s approvalSpec
	if err := decodeSpec(spec, &s); err != nil {
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

func (b *Builder) auditFactory(_ context.Context, spec json.RawMessage) (tool.Middleware, error) {
	if len(spec) > 0 && string(spec) != "null" && string(spec) != "{}" {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q takes no spec", KindAudit))
	}
	if b.deps.AuditSink == nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"kind %q requires an AuditSink in config.Deps", KindAudit))
	}
	return middleware.Audit(b.deps.AuditSink), nil
}

// decodeSpec strictly decodes a factory's spec: unknown fields and
// trailing garbage are errors.
func decodeSpec(spec json.RawMessage, v any) error {
	if len(spec) == 0 {
		return errdefs.Validation(fmt.Errorf("spec is required"))
	}
	decoder := json.NewDecoder(bytes.NewReader(spec))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return errdefs.Validation(fmt.Errorf("invalid spec: %w", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("unexpected trailing data")
		}
		return errdefs.Validation(fmt.Errorf("invalid spec: %w", err))
	}
	return nil
}
