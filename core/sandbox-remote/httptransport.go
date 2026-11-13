package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/httpkit"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// maxErrorBody bounds how much of an error response the client reads. A server
// that answers with a novel in an error body must not be able to make the
// client allocate it.
const maxErrorBody = 64 << 10

// httpTransport is the HTTP/1.1 encoding of the protocol: one POST per
// operation, a JSON body, and the wire error envelope on a non-2xx answer.
type httpTransport struct {
	base    string
	token   string
	client  *http.Client
	timeout time.Duration
}

// HTTPOption configures the HTTP transport.
type HTTPOption func(*httpTransport)

// WithHTTPToken authenticates every request with a bearer token. The token
// travels only in the Authorization header: a query parameter would end up in
// proxy logs and access records.
func WithHTTPToken(token string) HTTPOption {
	return func(t *httpTransport) { t.token = token }
}

// WithHTTPClient replaces the default client. The default is built by
// core/internal/httpkit with retries disabled: a retry on this layer would
// duplicate a write, so the client retries only the operations that are safe to
// repeat, and it decides that per operation.
func WithHTTPClient(client *http.Client) HTTPOption {
	return func(t *httpTransport) {
		if client != nil {
			t.client = client
		}
	}
}

// WithHTTPTLS sets the client TLS configuration (custom roots, mTLS).
func WithHTTPTLS(config *tls.Config) HTTPOption {
	return func(t *httpTransport) {
		t.client = httpkit.NewHTTPClient(
			httpkit.WithTLSClientConfig(config),
			httpkit.WithoutRetry(),
		)
	}
}

// WithHTTPInsecureTLS disables certificate verification.
//
// It is named for what it does so it cannot be turned on absent-mindedly: it is
// for a loopback listener or a test server, never for a boundary that carries
// command execution.
func WithHTTPInsecureTLS() HTTPOption {
	return func(t *httpTransport) {
		t.client = httpkit.NewHTTPClient(
			httpkit.WithTLSClientConfig(&tls.Config{
				// Skipping verification is the point of this option; the
				// protocol floor is not, so it stays at 1.2.
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true, //nolint:gosec // opt-in by name, loopback only.
			}),
			httpkit.WithoutRetry(),
		)
	}
}

// WithHTTPTimeout bounds one request end to end. It must stay above the longest
// hold the client asks for, or a long poll would be cut short by the transport
// instead of answering.
func WithHTTPTimeout(timeout time.Duration) HTTPOption {
	return func(t *httpTransport) {
		if timeout > 0 {
			t.timeout = timeout
		}
	}
}

// NewHTTPTransport returns a Transport that speaks the protocol over HTTP/1.1 to
// base (for example "https://sandbox.internal:8443").
func NewHTTPTransport(base string, opts ...HTTPOption) Transport {
	t := &httpTransport{
		base:    strings.TrimRight(base, "/"),
		timeout: 2 * time.Minute,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	if t.client == nil {
		t.client = httpkit.NewHTTPClient(httpkit.WithoutRetry())
	}
	return t
}

// Do implements Transport.
func (t *httpTransport) Do(ctx context.Context, op Op, req []byte, res any) error {
	resp, err := t.open(ctx, op, req, "application/json")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if res == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return errdefs.Internalf("sandbox-remote: read %s response: %w", op, err)
	}
	if err := wire.Decode(respBody, res); err != nil {
		return errdefs.Internalf("sandbox-remote: decode %s response: %w", op, err)
	}
	return nil
}

// maxResponseBody bounds a successful response. Output batches are the largest
// payload and are already bounded by the read's max_bytes, so this ceiling only
// guards against a server that answers with something else.
const maxResponseBody = 64 << 20

// Stream implements Transport. It is Do without the body decoding: the response
// stays open and the caller reads frames from it until it closes or the context
// ends.
func (t *httpTransport) Stream(ctx context.Context, op Op, req []byte) (io.ReadCloser, error) {
	resp, err := t.open(ctx, op, req, "text/event-stream")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// open builds one request, applies authentication and trace propagation, and
// returns a 2xx response with its body still unread.
func (t *httpTransport) open(ctx context.Context, op Op, req []byte, accept string) (*http.Response, error) {
	url := t.base + "/v1/" + string(op)

	var body io.Reader
	if len(req) > 0 {
		body = bytes.NewReader(req)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, errdefs.Internalf("sandbox-remote: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", accept)
	if t.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+t.token)
	}
	// The command a sandbox runs belongs to the trace that asked for it.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(httpReq.Header))

	client := t.client
	if t.timeout > 0 {
		// Shallow copy: the deadline belongs to this call, not to the shared
		// client (which is reused across sessions).
		clone := *client
		clone.Timeout = t.timeout
		client = &clone
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errdefs.FromContext(ctxErr)
		}
		return nil, &TransportError{Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeFailure(resp)
	}
	return resp, nil
}

// decodeFailure rebuilds the error the server reported. A body that is not the
// envelope (a proxy's HTML error page, say) still has to produce an error whose
// classification reflects the status.
func decodeFailure(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var envelope wire.Error
	if err := wire.Decode(body, &envelope); err == nil && envelope.Message != "" {
		return envelope.Err()
	}
	return errdefs.ClassifyStatus(resp.StatusCode,
		errdefs.Internalf("sandbox-remote: server answered %d", resp.StatusCode))
}
