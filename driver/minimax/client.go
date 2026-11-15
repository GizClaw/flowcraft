package minimax

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/GizClaw/flowcraft/driver/internal/httpx"
	"github.com/goccy/go-json"
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
//
// The transport is `driver/internal/httpx`: the client, the body ceiling and the
// failure classification are shared with the other vendor drivers. Retries are
// not here — `core/inference/route` owns the attempt budget for every seam, so a
// call asks once and reports what happened.
type mediaClient struct {
	http *httpx.Client
	poll time.Duration
}

func newMediaClient(settings Settings) *mediaClient {
	return &mediaClient{
		http: httpx.New(httpx.Config{
			BaseURL:     settings.BaseURL,
			APIKey:      settings.APIKey,
			Headers:     settings.Headers,
			HTTP:        settings.HTTPClient,
			Timeout:     settings.Timeout,
			Limit:       unaryResponseLimit,
			DecodeError: envelopeFailure,
		}),
		poll: settings.VideoPollInterval,
	}
}

// pollInterval is the pacing video task polling uses. The settings are normalized
// before the client is built, so this only guards a zero value built by hand.
func (c *mediaClient) pollInterval() time.Duration {
	if c.poll <= 0 {
		return DefaultVideoPollInterval
	}
	return c.poll
}

// envelopeFailure reads MiniMax's base_resp out of a failure body. The v1 media
// surfaces report most failures on an HTTP 200 — where this hook never runs,
// because the caller inspects the decoded envelope itself — and the v2 task API
// answers ordinary statuses that may still describe the failure this way.
func envelopeFailure(_ int, body []byte) error {
	var envelope struct {
		BaseResp baseResp `json:"base_resp"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.BaseResp.Code == 0 {
		return nil
	}
	return envelope.BaseResp.err("media call")
}

// postJSON performs one create call and decodes the answer. Every surface that
// uses it starts billed work (a video render, a synthesis), so the transport
// never repeats it: a re-sent create buys the work twice.
func (c *mediaClient) postJSON(ctx context.Context, path string, body, out any) error {
	return c.http.JSON(ctx, httpx.Op{Method: http.MethodPost, Path: path, Body: body}, out)
}

// getJSON performs one read — the task query path.
func (c *mediaClient) getJSON(ctx context.Context, target string, out any) error {
	return c.http.JSON(ctx, httpx.Op{Method: http.MethodGet, Path: target}, out)
}

// postSSE opens a streaming synthesis and hands back the event-stream body for
// the caller to read; the caller closes it.
func (c *mediaClient) postSSE(ctx context.Context, path string, body any) (io.ReadCloser, error) {
	return c.http.Stream(ctx, httpx.Op{Method: http.MethodPost, Path: path, Body: body})
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
