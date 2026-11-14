package mcp

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// MCP has a lifecycle of its own: a server is connected, its tools are published,
// and a dead connection is retried in the background. None of that shows up in a
// tool span, because the call may never happen — the connection died first. This
// file instruments the connection side.

// attrMCPTransport names the transport implementation (stdio, streamable HTTP).
const attrMCPTransport = "mcp.transport"

// startConnect opens the span for one server connection attempt and returns the
// transport label the metrics share with it, so a span and its counter cannot
// disagree about which transport was used.
func startConnect(
	ctx context.Context,
	server string,
	transport any,
) (context.Context, trace.Span, string) {
	label := fmt.Sprintf("%T", transport)
	ctx, span := telemetry.TracerWithSuffix("mcp").Start(ctx, "mcp.connect",
		trace.WithAttributes(
			attribute.String(telemetry.AttrMCPServer, server),
			attribute.String(attrMCPTransport, label),
		))
	return ctx, span, label
}

// finishConnect closes the attempt's span and records it.
//
// The status is a label, never the endpoint's message: a failing handshake can
// echo a credential in its error, and the warn log at the call site already
// carries the detail.
func finishConnect(
	ctx context.Context,
	span trace.Span,
	server, transport string,
	err error,
) {
	if err != nil {
		span.SetStatus(codes.Error, "connect_failed")
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
	recordConnect(ctx, server, transport, err)
}

// recordConnect counts one connection attempt by outcome.
func recordConnect(ctx context.Context, server, transport string, err error) {
	meter := telemetry.MeterWithSuffix("mcp")
	status := "success"
	if err != nil {
		status = "error"
	}
	if counter, err := meter.Int64Counter("connects.total",
		metric.WithDescription("MCP server connection attempts by outcome")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(
			attribute.String(telemetry.AttrMCPServer, server),
			attribute.String(attrMCPTransport, transport),
			attribute.String("status", status)))
	}
}

// recordReconnect counts a scheduled reconnect: the attempt itself happens later,
// but the decision is what explains a gap in tool availability.
func recordReconnect(ctx context.Context, server string) {
	meter := telemetry.MeterWithSuffix("mcp")
	if counter, err := meter.Int64Counter("reconnects.total",
		metric.WithDescription("Scheduled MCP reconnects")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(
			attribute.String(telemetry.AttrMCPServer, server)))
	}
}

// recordServerLost counts a connection the transport reported dead.
func recordServerLost(ctx context.Context, server string) {
	meter := telemetry.MeterWithSuffix("mcp")
	if counter, err := meter.Int64Counter("server_lost.total",
		metric.WithDescription("MCP connections reported dead by the transport")); err == nil {
		counter.Add(ctx, 1, metric.WithAttributes(
			attribute.String(telemetry.AttrMCPServer, server)))
	}
}
