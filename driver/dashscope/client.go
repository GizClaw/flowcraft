package dashscope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils"
)

// The protocol headers the platform switches behaviour on. async is
// mandatory on every task-creating call — the media endpoints refuse a
// synchronous call — and sse is mandatory on the interleaved sync surface,
// whose endpoint refuses a non-streaming call. workspace rides along only
// when the deployment declares one.
const (
	headerAsync     = "X-DashScope-Async"
	headerSSE       = "X-DashScope-Sse"
	headerWorkspace = "X-DashScope-WorkSpace"
)

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

// profileMaterial is one profile's resolved credentials and profile-level
// settings, validated once at factory build time.
type profileMaterial struct {
	spec     ProfileSpec
	apiKey   resource.Secret
	resolver *resource.SecretResolver
}

// clients carries the handles one profile opens drivers with. Every surface
// this driver serves is a plain JSON API, so one HTTP client rooted at the
// profile's host is all it takes.
type clients struct {
	media *mediaClient
}

func newProfileMaterial(
	ctx context.Context,
	profile ProfileSettings,
	secrets *resource.SecretResolver,
) (profileMaterial, error) {
	spec, err := decodeProfileSpec(ctx, profile.Spec)
	if err != nil {
		return profileMaterial{}, fmt.Errorf("profile %q: %w", profile.ID, err)
	}
	material := profileMaterial{spec: spec, resolver: secrets}
	for name := range profile.Secrets {
		switch name {
		case SecretAPIKey:
		default:
			return profileMaterial{}, fmt.Errorf(
				"profile %q carries unknown secret %q", profile.ID, name)
		}
	}
	if secret, ok := profile.Secrets[SecretAPIKey]; ok {
		material.apiKey = secret
	}
	return material, nil
}

func (m profileMaterial) newClients(ctx context.Context, spec Spec) (*clients, error) {
	apiKey, err := m.apiKey.Resolve(ctx, m.resolver)
	if err != nil {
		return nil, errdefs.Validationf("dashscope profile: resolve api_key: %w", err)
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errdefs.Validationf("dashscope profile resolves no api_key secret")
	}
	base := spec.baseURL()
	if m.spec.BaseURL != "" {
		base = strings.TrimRight(m.spec.BaseURL, "/")
	}
	workspace := spec.WorkspaceID
	if m.spec.WorkspaceID != "" {
		workspace = m.spec.WorkspaceID
	}
	return &clients{
		media: newMediaClient(apiKey, base, workspace, spec),
	}, nil
}

// mediaClient speaks the DashScope native API: plain JSON over HTTP with
// Bearer auth, the async header on task surfaces, and the optional
// workspace header.
type mediaClient struct {
	http      *http.Client
	key       string
	base      string
	workspace string
}

func newMediaClient(key, base, workspace string, spec Spec) *mediaClient {
	options := []utils.Option{
		utils.WithTimeout(10 * time.Minute),
		utils.WithResponseHeaderTimeout(5 * time.Minute),
	}
	if spec.HTTPRetries != nil {
		options = append(options, utils.WithRetryAttempts(int(*spec.HTTPRetries)))
	}
	return &mediaClient{
		http:      utils.NewHttpClient(options...),
		key:       key,
		base:      strings.TrimRight(base, "/"),
		workspace: workspace,
	}
}

// requestOptions carries the protocol switches a call asks for: async on the
// task-creating calls, sse on the streamed ones.
type requestOptions struct {
	async bool
	sse   bool
}

// request performs one wire call. async marks a task-creating call, which
// must carry X-DashScope-Async: enable — the platform rejects synchronous
// media calls with "current user api does not support synchronous calls".
// sse marks a streamed call: the interleaved surface requires
// X-DashScope-Sse: enable, which turns the answer into a sequence of events.
func (c *mediaClient) request(
	ctx context.Context,
	method, path string,
	body any,
	opts requestOptions,
) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("dashscope: marshal request: %w", err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		return nil, fmt.Errorf("dashscope: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if opts.async {
		req.Header.Set(headerAsync, "enable")
	}
	if opts.sse {
		req.Header.Set(headerSSE, "enable")
	}
	if c.workspace != "" {
		req.Header.Set(headerWorkspace, c.workspace)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dashscope: %s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var envelope errorBody
		_ = json.Unmarshal(snippet, &envelope)
		detail := strings.TrimSpace(string(snippet))
		if envelope.Code != "" || envelope.Message != "" {
			detail = nonEmpty(envelope.Code, "HTTP") + ": " + nonEmpty(envelope.Message, detail)
		}
		failure := classifyStatus(resp.StatusCode, envelope.Code, fmt.Errorf(
			"dashscope: %s %s: HTTP %d: %s", method, path, resp.StatusCode, detail,
		))
		if envelope.RequestID != "" {
			failure = errdefs.WithRequestID(failure, envelope.RequestID)
		}
		return nil, errdefs.WithRetryCount(
			errdefs.WithRetryAfter(
				failure,
				errdefs.ParseRetryAfter(resp.Header.Get("Retry-After")),
			),
			utils.RetryCountOf(resp),
		)
	}
	return resp, nil
}

// postJSON decodes a JSON response into out; the caller inspects the
// envelope's code field, because the platform also reports task-level
// failures on an HTTP 200 create response.
func (c *mediaClient) postJSON(
	ctx context.Context,
	path string,
	async bool,
	body, out any,
) error {
	resp, err := c.request(ctx, http.MethodPost, path, body, requestOptions{async: async})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("dashscope: decode %s response: %w", path, err)
	}
	return nil
}

// getJSON decodes a JSON response into out; the task query path and the
// failure paths share it.
func (c *mediaClient) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.request(ctx, http.MethodGet, path, nil, requestOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("dashscope: decode %s response: %w", path, err)
	}
	return nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, fmt.Errorf("dashscope: build result request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dashscope: fetch result file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, errdefs.ClassifyStatus(resp.StatusCode, fmt.Errorf(
			"dashscope: fetch result file: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)),
		))
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, artifactLimit+1))
	if err != nil {
		return nil, fmt.Errorf("dashscope: read result file: %w", err)
	}
	if len(payload) > artifactLimit {
		return nil, fmt.Errorf(
			"dashscope: result file exceeds %d bytes", artifactLimit)
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
	resp, err := c.request(ctx, http.MethodPost, path, body, requestOptions{sse: sse})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, sseResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("dashscope: read %s response: %w", path, err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("dashscope: %s returned an empty response", path)
	}
	if json.Valid(trimmed) {
		return [][]byte{trimmed}, nil
	}
	return sseEvents(trimmed, path)
}

// sseEvents splits a streamed body into one payload per event. Data lines
// accumulate until a blank line closes the event, the way the event-stream
// format specifies; the framing fields a JSON payload never needs —
// heartbeats and comments, id, event, and retry — are skipped.
func sseEvents(raw []byte, path string) ([][]byte, error) {
	var (
		events  [][]byte
		payload []string
	)
	flush := func() {
		if len(payload) == 0 {
			return
		}
		events = append(events, []byte(strings.Join(payload, "\n")))
		payload = nil
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		switch {
		case trimmed == "":
			flush()
		case strings.HasPrefix(trimmed, "data:"):
			fragment := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if fragment == "" || fragment == "[DONE]" {
				continue
			}
			payload = append(payload, fragment)
		case strings.HasPrefix(trimmed, ":"),
			strings.HasPrefix(trimmed, "event:"),
			strings.HasPrefix(trimmed, "id:"),
			strings.HasPrefix(trimmed, "retry:"):
			// Framing without content: a heartbeat, a comment, or the
			// stream's event name.
		default:
			return nil, fmt.Errorf(
				"dashscope: %s returned an unrecognized event line %q",
				path, trimmed,
			)
		}
	}
	flush()
	if len(events) == 0 {
		return nil, fmt.Errorf("dashscope: %s returned no events", path)
	}
	return events, nil
}
