// Package httpx is the drivers' shared wire layer: one client, one request
// builder, one bounded JSON decoder, one event-stream reader and one task
// poller.
//
// It exists because three vendor drivers were each writing the same transport by
// hand — the client, the bearer header, the body ceiling, the SSE framing, the
// async polling — with the numbers drifting between them. Nothing here is
// vendor-specific: an envelope a vendor carries on an HTTP 200 arrives through
// [Config.DecodeError], and everything else a vendor knows stays in its own
// package.
//
// # Retries are not here
//
// A call performs one attempt. Retrying belongs to `core/inference/route`, which
// already owns the attempt budget, the per-target limit and the backoff schedule
// for every seam; a transport that counted attempts too would be a second
// budget nobody can reason about. What a transport owes the layer above is the
// classification: [Client.Do] returns the failure already mapped to
// `errdefs`, with the server's `Retry-After` hint and any request id attached, so
// the router can decide.
package httpx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/goccy/go-json"
)

// DefaultLimit bounds one response body when a call does not say otherwise. A
// driver that serves whole media files sets a larger one per call: the ceiling
// exists so a broken gateway cannot decide how much memory the driver allocates.
const DefaultLimit int64 = 8 << 20

// errorSnippet bounds how much of a failed response body rides the error. The
// text is the provider's, it lands in logs and in caller-visible errors, and an
// over-long one must not become the driver's problem.
const errorSnippet = 4096

// Config is the deployment's side of the client: where the API is, how it
// authenticates, and the ceiling an unmetered call gets.
type Config struct {
	// BaseURL is the API root, without a trailing slash.
	BaseURL string
	// APIKey is sent as a bearer token when non-empty.
	APIKey string
	// Headers are applied last, for a vendor that needs a fixed extra header.
	Headers map[string]string
	// HTTP overrides the client. Nil builds one from Timeout.
	HTTP *http.Client
	// Timeout bounds one request when HTTP is nil.
	Timeout time.Duration
	// Limit bounds a response body a call does not meter.
	Limit int64
	// DecodeError reads a failure out of a response body before the status is
	// classified. It returns nil when it cannot say more than the status.
	//
	// It exists because a vendor may answer 200 with a failure envelope, or
	// describe a failure in the body of a 4xx; either way the classification is
	// the vendor's knowledge, and the transport only carries it.
	DecodeError func(status int, body []byte) error
}

// Client speaks one API root. It is safe for concurrent use and owns no state
// beyond its configuration.
type Client struct {
	http    *http.Client
	base    string
	key     string
	headers map[string]string
	limit   int64
	decode  func(status int, body []byte) error
}

// New builds a client from a configuration.
func New(config Config) *Client {
	client := config.HTTP
	if client == nil {
		client = &http.Client{Timeout: config.Timeout}
	}
	limit := config.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	return &Client{
		http:    client,
		base:    strings.TrimRight(config.BaseURL, "/"),
		key:     strings.TrimSpace(config.APIKey),
		headers: config.Headers,
		limit:   limit,
		decode:  config.DecodeError,
	}
}

// Op is one wire call.
type Op struct {
	Method string
	// Path is appended to the base URL. It may be absolute for a poller that
	// follows a URL the provider handed back.
	Path    string
	Body    any
	Headers map[string]string
	// Limit overrides the client's body ceiling for this call.
	Limit int64
}

// Do performs one attempt and returns the response with a 2xx status. Every
// other outcome is an error already classified for the layer above: the vendor
// envelope first (when configured), then the status, with `Retry-After` and the
// request id attached when the response carried them.
//
// The caller owns the returned body.
func (c *Client) Do(ctx context.Context, op Op) (*http.Response, error) {
	target := op.Path
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = c.base + target
	}
	var payload []byte
	if op.Body != nil {
		raw, err := json.Marshal(op.Body)
		if err != nil {
			return nil, errdefs.Validationf("request body: %w", err)
		}
		payload = raw
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, op.Method, target, reader)
	if err != nil {
		return nil, errdefs.Validationf("build request: %w", err)
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range c.headers {
		req.Header.Set(name, value)
	}
	for name, value := range op.Headers {
		req.Header.Set(name, value)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A cancellation is the caller's, not the far end's: reporting it as a
		// transport failure would let a retry layer repeat abandoned work.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errdefs.Interrupted(fmt.Errorf("%s %s: %w", op.Method, op.Path, ctxErr))
		}
		return nil, errdefs.NotAvailable(fmt.Errorf("%s %s: %w", op.Method, op.Path, err))
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return resp, nil
	}

	defer func() { _ = resp.Body.Close() }()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errorSnippet))
	return nil, c.failure(op, resp, snippet)
}

// failure classifies a non-2xx response.
func (c *Client) failure(op Op, resp *http.Response, body []byte) error {
	base := fmt.Errorf("%s %s: HTTP %d: %s",
		op.Method, op.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	var failure error
	if c.decode != nil {
		// The vendor's own envelope knows more than the status does: a 200 that
		// carries a failure, or a 4xx whose body names the reason.
		failure = c.decode(resp.StatusCode, body)
	}
	if failure == nil {
		failure = errdefs.ClassifyStatus(resp.StatusCode, base)
	}
	return errdefs.WithRetryAfter(failure, errdefs.ParseRetryAfter(resp.Header.Get("Retry-After")))
}

// JSON performs one call and decodes its bounded body into out.
//
// The ceiling is read off the wire with one byte to spare, so an oversized body
// is reported as oversized rather than as a truncated payload that fails to
// parse.
func (c *Client) JSON(ctx context.Context, op Op, out any) error {
	resp, err := c.Do(ctx, op)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	limit := op.Limit
	if limit <= 0 {
		limit = c.limit
	}
	return DecodeJSON(op.Path, resp.Body, limit, out)
}

// DecodeJSON reads at most limit bytes and decodes them.
func DecodeJSON(where string, body io.Reader, limit int64, out any) error {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return fmt.Errorf("read %s response: %w", where, err)
	}
	if int64(len(raw)) > limit {
		return inference.NewError(
			inference.InvalidProviderResponse, inference.FieldOutput,
			fmt.Errorf("%s response exceeds the %d-byte limit", where, limit))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return inference.NewError(
			inference.InvalidProviderResponse, inference.FieldOutput,
			fmt.Errorf("decode %s response: %w", where, err))
	}
	return nil
}

// Bytes performs one call and returns its body, refusing a payload over limit.
//
// The ceiling is the caller's because it is the caller's data: a signed artifact
// link is not a provider response, and how large a result file may be is a
// property of the surface, not of the transport.
func (c *Client) Bytes(ctx context.Context, op Op, limit int64) ([]byte, error) {
	resp, err := c.Do(ctx, op)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if limit <= 0 {
		limit = c.limit
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", op.Path, err)
	}
	if int64(len(raw)) > limit {
		return nil, inference.NewError(
			inference.InvalidProviderResponse, inference.FieldOutput,
			fmt.Errorf("%s response exceeds the %d-byte limit", op.Path, limit))
	}
	return raw, nil
}

// Stream performs one call and hands back its event-stream body. The caller
// closes it.
func (c *Client) Stream(ctx context.Context, op Op) (io.ReadCloser, error) {
	resp, err := c.Do(ctx, op)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
