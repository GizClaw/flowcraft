// Package httpprobe wraps an HTTP transport with an opt-in round-trip
// probe.
//
// It answers one question the per-turn latency reports cannot: how much
// of a slow inference step is local assembly (engine bookkeeping,
// request encoding, connection setup) and how much is the provider
// (upload, time to first byte, generation). With the probe in place,
// one round trip emits two records:
//
//	httpprobe: request dispatched        (the request bytes left the process)
//	httpprobe: response headers received  (status + the wait so far)
//
// The gap from the previous host log to "request dispatched" is local
// work; "request dispatched" to "response headers received" is network
// plus provider time to first byte. Both records carry the trace id the
// telemetry layer injects from the caller's context, so they join the
// driver's lines.
//
// [New] is the primary shape: the caller wraps the transport it is
// about to hand to a client, so there is no install window and no
// global state:
//
//	client := &http.Client{Transport: httpprobe.New(base)}
//
// [Install] is the escape hatch for callers that cannot reach the
// client construction (the provider SDKs read http.DefaultTransport
// when they build their client): it wraps the process transport at
// most once, and the application owns the opt-in ([Enabled] reads
// FLOWCRAFT_HTTP_PROBE). Only request body sizes are measured, never
// logged, and only POST/PUT/PATCH round trips are recorded, which
// keeps the output to provider traffic.
//
// Two notes on the global form. A wrapped process transport is not
// transparent to every caller — core/utils builds provider transports
// by cloning http.DefaultTransport through a *http.Transport
// assertion, and that path falls back to a fresh transport instead of
// panicking once the probe is installed — and a client that received
// the wrapper before [Uninstall] goes quiet, because RoundTrip reads
// the enabled flag per call instead of capturing it.
package httpprobe

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// Env enables the probe: any value strconv.ParseBool accepts turns it
// on. Callers own the opt-in; nothing here installs the probe unasked.
const Env = "FLOWCRAFT_HTTP_PROBE"

// Enabled reports whether the environment asks for the probe.
func Enabled() bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(Env)))
	return err == nil && value
}

// Probe decorates a RoundTripper with the two boundary records. It is
// safe for concurrent use.
type Probe struct {
	base    http.RoundTripper
	enabled atomic.Bool
}

// New wraps base with the probe, enabled. Disable quiets the wrapper
// for clients that already hold it.
func New(base http.RoundTripper) *Probe {
	p := &Probe{base: base}
	p.enabled.Store(true)
	return p
}

// Disable stops recording. RoundTrip reads the flag per call, so
// turning it off also quiets transports that have already been handed
// out.
func (p *Probe) Disable() { p.enabled.Store(false) }

// RoundTrip records the request and response-header boundaries while
// the probe is enabled; everything else passes through untouched, and
// the transport's own error is returned unchanged alongside the
// failure record.
func (p *Probe) RoundTrip(req *http.Request) (*http.Response, error) {
	if !p.enabled.Load() || !probed(req) {
		return p.base.RoundTrip(req)
	}
	ctx := req.Context()
	attrs := []attribute.KeyValue{
		attribute.String("http.method", req.Method),
		attribute.String("http.host", req.URL.Host),
		attribute.String("http.path", req.URL.Path),
		attribute.Int64("request_bytes", requestBytes(req)),
	}
	telemetry.Info(ctx, "httpprobe: request dispatched", attrs...)

	started := time.Now()
	resp, err := p.base.RoundTrip(req)
	waited := time.Since(started)
	if err != nil {
		telemetry.WarnErr(ctx, "httpprobe: request failed",
			fmt.Errorf("%s %s after %s: %w", req.Method, req.URL.Host,
				waited.Round(time.Millisecond), err),
			attrs...)
		return nil, err
	}
	telemetry.Info(ctx, "httpprobe: response headers received",
		attribute.String("http.status", strconv.Itoa(resp.StatusCode)),
		attribute.Int64("wait_ms", waited.Milliseconds()),
		attribute.Bool("content_length_known", resp.ContentLength >= 0),
		attribute.String("content_type", resp.Header.Get("Content-Type")))
	return resp, nil
}

var (
	globalMu    sync.Mutex
	globalProbe *Probe
)

// Install wraps http.DefaultTransport so round trips through clients
// built afterwards are recorded, and reports whether the probe is in
// place. It is idempotent; the environment or the application decides
// whether to call it at all.
func Install() bool {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalProbe != nil {
		return true
	}
	globalProbe = New(http.DefaultTransport)
	http.DefaultTransport = globalProbe
	return true
}

// Uninstall restores the transport the probe wrapped and reports
// whether one was in place. The wrapper itself goes quiet, so clients
// that already captured it stop recording even though they keep
// holding it.
func Uninstall() bool {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalProbe == nil {
		return false
	}
	if wrapped, ok := http.DefaultTransport.(*Probe); ok && wrapped == globalProbe {
		http.DefaultTransport = globalProbe.base
	}
	globalProbe.Disable()
	globalProbe = nil
	return true
}

// Active reports whether the probe is installed on the process
// transport.
func Active() bool {
	globalMu.Lock()
	defer globalMu.Unlock()
	return globalProbe != nil
}

// probed reports whether one request is worth recording: a
// body-carrying write, which is what every inference call is. GETs
// (model lists, MCP discovery) are cheap and would only add noise, and
// the application's own OTLP export is a POST too, so the collector
// paths stay out of the record.
func probed(req *http.Request) bool {
	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return false
	}
	if strings.Contains(req.URL.Path, "/v1/") {
		switch path.Base(req.URL.Path) {
		case "logs", "traces", "metrics":
			return false
		}
	}
	return true
}

// requestBytes reports the declared body size, or -1 when the request
// has no measurable body. The body itself is never read or logged: the
// driver may stream it, and a request body holds conversation content.
func requestBytes(req *http.Request) int64 {
	switch {
	case req.ContentLength > 0:
		return req.ContentLength
	case req.Body != nil:
		return -1
	default:
		return 0
	}
}
