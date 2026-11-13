package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
)

// retryBackoffBase is the first wait between wire attempts; it doubles per
// attempt up to retryBackoffMax. A server Retry-After hint wins when present.
const (
	retryBackoffBase = 500 * time.Millisecond
	retryBackoffMax  = 8 * time.Second
	// retryAfterMax caps a server hint: waiting longer than this is the caller's
	// decision, not the transport's.
	retryAfterMax = 30 * time.Second
	// retryBudget bounds the total time one call may spend waiting between
	// attempts. A configured retry count is an attempt budget, not a licence to
	// stall for an hour, and the video poll re-enters this path on every
	// iteration.
	retryBudget = 60 * time.Second
)

// unaryResponseLimit bounds one JSON response body. A unary speech answer carries
// the whole utterance as hex — twice its decoded size — so the ceiling is
// generous but finite: an unbounded decode would let a broken gateway decide how
// much memory the driver allocates.
const unaryResponseLimit = 32 << 20

// mediaClient speaks MiniMax's media APIs (t2a and the video task pipeline):
// plain JSON over HTTP with Bearer auth and — on the v1 surfaces — a base_resp
// status envelope carried on an HTTP 200. There is no vendor SDK for these
// endpoints, and adding one would only wrap the same JSON.
type mediaClient struct {
	http    *http.Client
	key     string
	base    string
	headers map[string]string
	poll    time.Duration
	retries int
	// budget overrides retryBudget when positive; tests shrink it so a
	// budget-exhausted path does not take a minute to prove.
	budget time.Duration
}

func newMediaClient(settings Settings) *mediaClient {
	client := settings.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: settings.Timeout}
	}
	return &mediaClient{
		http:    client,
		key:     strings.TrimSpace(settings.APIKey),
		base:    strings.TrimRight(settings.BaseURL, "/"),
		headers: settings.Headers,
		poll:    settings.VideoPollInterval,
		retries: settings.Retries,
	}
}

// pollInterval is the pacing video task polling uses. The settings are
// normalized before the client is built, so this only guards a zero value built
// by hand.
func (c *mediaClient) pollInterval() time.Duration {
	if c.poll <= 0 {
		return DefaultVideoPollInterval
	}
	return c.poll
}

// retryBudgetOf is the total time one call may spend waiting between attempts.
func (c *mediaClient) retryBudgetOf() time.Duration {
	if c.budget > 0 {
		return c.budget
	}
	return retryBudget
}

// request performs one wire call. A read-only call may retry transient failures
// up to the configured budget; a call that creates something never does, because
// re-sending an accepted create request buys the work twice — and every paid
// surface here is a create (a video task, a speech synthesis). Retrying is a
// transport concern only when the route Router owns the budget (count zero) and
// only for requests that are safe to repeat.
func (c *mediaClient) request(
	ctx context.Context,
	method, path string,
	body any,
	idempotent bool,
) (*http.Response, error) {
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("minimax: marshal request: %w", err)
		}
		payload = raw
	}
	attempts := 1
	if idempotent {
		attempts = c.retries + 1
	}
	var waited time.Duration
	for attempt := 0; ; attempt++ {
		resp, err := c.send(ctx, method, path, payload)
		if err == nil {
			return resp, err
		}
		if attempt+1 >= attempts || !retryable(err) {
			return nil, err
		}
		wait := retryWait(err, attempt)
		if waited+wait > c.retryBudgetOf() {
			// The next attempt would exceed the transport's whole waiting
			// budget: hand the failure back while it is still the provider's
			// error, rather than after the caller's deadline turns it into an
			// interruption.
			return nil, err
		}
		waited += wait
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
	}
}

// send performs one attempt. MiniMax answers failures two ways: the v1 media
// surfaces return HTTP 200 with a populated base_resp, and the v2 task API
// answers ordinary HTTP status codes. The second arrives here as a non-2xx
// response, classified by status and refined by whatever envelope the body
// carries.
func (c *mediaClient) send(
	ctx context.Context,
	method, path string,
	payload []byte,
) (*http.Response, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("minimax: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range c.headers {
		req.Header.Set(name, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errdefs.Interrupted(fmt.Errorf("minimax: %s %s: %w", method, path, ctxErr))
		}
		return nil, errdefs.NotAvailable(fmt.Errorf("minimax: %s %s: %w", method, path, err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		failure := classifyTransport(resp.StatusCode, snippet, fmt.Errorf(
			"minimax: %s %s: HTTP %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(snippet)),
		))
		if id := requestIDOf(snippet); id != "" {
			failure = errdefs.WithRequestID(failure, id)
		}
		return nil, errdefs.WithRetryAfter(
			failure,
			errdefs.ParseRetryAfter(resp.Header.Get("Retry-After")),
		)
	}
	return resp, nil
}

// retryable reports whether another wire attempt can plausibly succeed. A
// validation or auth failure is deterministic, so retrying it only spends the
// caller's budget.
func retryable(err error) bool {
	return errdefs.IsRateLimit(err) ||
		errdefs.IsNotAvailable(err) ||
		errdefs.IsTimeout(err) ||
		errdefs.IsInternal(err)
}

// retryWait is the pause before the next attempt: the server's hint when it
// gave one, an exponential backoff otherwise.
func retryWait(err error, attempt int) time.Duration {
	if hint, ok := errdefs.RetryAfter(err); ok {
		if hint > retryAfterMax {
			return retryAfterMax
		}
		return hint
	}
	wait := retryBackoffBase << attempt
	if wait > retryBackoffMax || wait <= 0 {
		return retryBackoffMax
	}
	return wait
}

// postJSON decodes a JSON response into out; the caller inspects the envelope's
// base_resp, because the v1 surfaces report failures on an HTTP 200. It is a
// create on every surface that uses it, so it is never retried.
func (c *mediaClient) postJSON(ctx context.Context, path string, body, out any) error {
	resp, err := c.request(ctx, http.MethodPost, path, body, false)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeJSONBody(path, resp, out)
}

// getJSON decodes a JSON response into out; the task query path uses it. It is a
// read, so it carries the retry budget.
func (c *mediaClient) getJSON(ctx context.Context, target string, out any) error {
	resp, err := c.request(ctx, http.MethodGet, target, nil, true)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeJSONBody(target, resp, out)
}

// decodeJSONBody reads one bounded response and decodes it.
func decodeJSONBody(endpoint string, resp *http.Response, out any) error {
	return decodeJSONWithin(endpoint, resp.Body, unaryResponseLimit, out)
}

// decodeJSONWithin reads at most limit bytes and decodes them. The ceiling is read
// off the wire with one byte to spare, so an oversized chunked body is reported as
// oversized rather than as a truncated payload.
func decodeJSONWithin(endpoint string, body io.Reader, limit int64, out any) error {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return fmt.Errorf("minimax: read %s response: %w", endpoint, err)
	}
	if int64(len(raw)) > limit {
		return inference.NewError(
			inference.InvalidProviderResponse, inference.FieldOutput,
			fmt.Errorf("minimax: %s response exceeds the %d-byte limit", endpoint, limit))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return inferenceResponseError(endpoint, err)
	}
	return nil
}

// postSSE posts a streaming request and hands back the event-stream body for
// the caller to scan; the caller closes it. Like postJSON it opens a billed
// synthesis, so it is never retried.
func (c *mediaClient) postSSE(ctx context.Context, path string, body any) (io.ReadCloser, error) {
	resp, err := c.request(ctx, http.MethodPost, path, body, false)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// requestIDOf reads a request identifier out of an error body without insisting
// on a shape: the v1 envelope calls it request_id on errors and trace_id on
// successes.
func requestIDOf(body []byte) string {
	var envelope struct {
		RequestID string `json:"request_id"`
		TraceID   string `json:"trace_id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	if envelope.RequestID != "" {
		return envelope.RequestID
	}
	return envelope.TraceID
}
