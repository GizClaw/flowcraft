package dashscope

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/inference/internal/httpx"
	"github.com/goccy/go-json"
)

// The protocol headers the platform switches behaviour on. async is mandatory on
// every task-creating call — the media endpoints refuse a synchronous call — and
// sse is mandatory on the interleaved sync surface, whose endpoint refuses a
// non-streaming call. workspace rides along only when the deployment declares one.
const (
	headerAsync     = "X-DashScope-Async"
	headerSSE       = "X-DashScope-Sse"
	headerWorkspace = "X-DashScope-WorkSpace"
)

// defaultTimeout bounds one wire call. A media task is answered immediately, but
// a long transcription poll is not.
const defaultTimeout = 10 * time.Minute

// ProfileSpec overrides provider-level transport facts for one credential
// profile. DashScope API keys are bound to the region they were created in,
// so a deployment that serves two regions declares one profile per host and
// pins the key that belongs to it.
type ProfileSpec struct {
	// BaseURL overrides the provider-wide base_url for this profile.
	BaseURL string `json:"base_url,omitempty"`
	// WorkspaceID overrides the provider-wide workspace_id for this
	// profile.
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// Validate checks the profile spec for structural sanity.
func (p ProfileSpec) Validate() error {
	if p.BaseURL != "" {
		if err := validateURL("base_url", p.BaseURL); err != nil {
			return err
		}
	}
	if p.WorkspaceID != "" && !workspaceIDPattern.MatchString(p.WorkspaceID) {
		return fmt.Errorf("invalid workspace_id %q", p.WorkspaceID)
	}
	return nil
}

// clients carries the handles one deployment opens drivers with.
type clients struct {
	media *mediaClient
}

func newClients(settings Spec) (*clients, error) {
	base := settings.baseURL()
	return &clients{media: newMediaClient(settings.APIKey, base, settings.WorkspaceID, settings)}, nil
}

// mediaClient speaks the DashScope native API: plain JSON over HTTP with Bearer
// auth, the async header on task surfaces, and the optional workspace header.
//
// Two clients, because one host must not see the other's credential: the API
// client carries the account key, and the file client fetches the signed result
// links a finished task names. Those links live on the platform's artifact host
// and carry their own signature, so sending the key there would spread it for
// nothing.
//
// The transport itself is `driver/internal/httpx`; retries are not here — the
// route Router owns the attempt budget for every seam.
type mediaClient struct {
	api   *httpx.Client
	files *httpx.Client
	poll  time.Duration
}

func newMediaClient(key, base, workspace string, spec Spec) *mediaClient {
	var headers map[string]string
	if workspace != "" {
		headers = map[string]string{headerWorkspace: workspace}
	}
	transport := spec.HTTPClient
	if transport == nil {
		transport = &http.Client{Timeout: defaultTimeout}
	}
	return &mediaClient{
		api: httpx.New(httpx.Config{
			BaseURL:     base,
			APIKey:      key,
			Headers:     headers,
			HTTP:        transport,
			Limit:       artifactLimit,
			DecodeError: envelopeFailure,
		}),
		files: httpx.New(httpx.Config{HTTP: transport, Limit: artifactLimit}),
		poll:  spec.pollInterval(),
	}
}

// pollInterval is the pacing task polling uses.
func (c *mediaClient) pollInterval() time.Duration {
	if c.poll <= 0 {
		return defaultPollInterval
	}
	return c.poll
}

// requestOptions carries the protocol switches a call asks for: async on the
// task-creating calls, sse on the streamed ones.
type requestOptions struct {
	async bool
	sse   bool
}

// op renders one call, adding the headers the platform switches behaviour on.
func (c *mediaClient) op(method, path string, body any, opts requestOptions) httpx.Op {
	call := httpx.Op{Method: method, Path: path, Body: body}
	switch {
	case opts.async:
		call.Headers = map[string]string{headerAsync: "enable"}
	case opts.sse:
		call.Headers = map[string]string{headerSSE: "enable"}
	}
	return call
}

// envelopeFailure reads DashScope's error envelope out of a failure body, so the
// classification names the platform's reason rather than only the status.
func envelopeFailure(_ int, body []byte) error {
	var envelope errorBody
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	if envelope.Code == "" && envelope.Message == "" {
		return nil
	}
	return classifyStatus(0, envelope.Code, fmt.Errorf("%s: %s",
		strings.TrimSpace(envelope.Code), strings.TrimSpace(envelope.Message)))
}

// postJSON performs one create call. async marks a task-creating call, which must
// carry X-DashScope-Async: enable — the platform rejects a synchronous media call
// with "current user api does not support synchronous calls".
func (c *mediaClient) postJSON(
	ctx context.Context,
	path string,
	async bool,
	body, out any,
) error {
	return c.api.JSON(ctx, c.op(http.MethodPost, path, body, requestOptions{async: async}), out)
}

// artifactLimit bounds one downloaded result file. A transcription result for
// hours of audio is large, but nowhere near this.
const artifactLimit = 64 << 20

// getArtifact downloads one result link — the transcription_url the terminal
// task query names. The link is signed and lives on the platform's result
// host, so the call carries no credential: the signature is in the URL, and
// sending the account key to a host that never asked for it would spread the
// credential for nothing.
func (c *mediaClient) getArtifact(ctx context.Context, link string) ([]byte, error) {
	parsed, err := url.Parse(link)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf(
			"dashscope: result link %q is not an http(s) URL", link)
	}
	payload, err := c.files.Bytes(ctx, httpx.Op{Method: http.MethodGet, Path: link}, artifactLimit)
	if err != nil {
		return nil, fmt.Errorf("dashscope: fetch result file: %w", err)
	}
	return payload, nil
}

// sseResponseLimit bounds one streamed response. The interleaved surface
// streams its content array as a sequence of small events; a run that reaches
// the cap is one that has gone wrong rather than one worth buffering.
const sseResponseLimit = 16 << 20

// postSSEEvents posts one multimodal call and returns its events. The
// interleaved surface requires X-DashScope-Sse: enable and answers with a
// sequence of server-sent events, each a JSON object carrying the run's
// content as far as it has come; every other sync call — and a deployment
// whose endpoint answers without framing — is one JSON object, returned as a
// single event so the caller folds both shapes the same way.
func (c *mediaClient) postSSEEvents(
	ctx context.Context,
	path string,
	body any,
	sse bool,
) ([][]byte, error) {
	raw, err := c.api.Bytes(ctx, c.op(http.MethodPost, path, body, requestOptions{sse: sse}), sseResponseLimit)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("dashscope: %s returned an empty response", path)
	}
	if json.Valid(trimmed) {
		return [][]byte{trimmed}, nil
	}
	var events [][]byte
	for frame, err := range httpx.Frames(bytes.NewReader(trimmed)) {
		if err != nil {
			return nil, fmt.Errorf("dashscope: %s: %w", path, err)
		}
		data := bytes.TrimSpace(frame.Data)
		if len(data) == 0 || string(data) == "[DONE]" {
			continue
		}
		events = append(events, data)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("dashscope: %s returned no events", path)
	}
	return events, nil
}
