package qwen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference/config"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit"

	otellog "go.opentelemetry.io/otel/log"
)

// Endpoint paths under the API root; the multimodal-generation endpoint
// serves the vision/video models, text-generation the rest.
const (
	pathTextGeneration       = "/api/v1/services/aigc/text-generation/generation"
	pathMultimodalGeneration = "/api/v1/services/aigc/multimodal-generation/generation"
)

// profileMaterial is one credential profile after secret resolution.
type profileMaterial struct {
	spec   ProfileSpec
	apiKey string
}

// dashClient speaks to DashScope's native generation API: JSON over HTTP
// with Bearer auth; streaming is SSE gated by the X-DashScope-SSE header.
type dashClient struct {
	http *http.Client
	key  string
	base string
}

func newProfileMaterial(profile config.ResolvedProfile) (profileMaterial, error) {
	spec, err := decodeProfileSpec(profile.Spec)
	if err != nil {
		return profileMaterial{}, err
	}
	material := profileMaterial{spec: spec}
	for name := range profile.Secrets {
		if name != SecretAPIKey {
			return profileMaterial{}, fmt.Errorf(
				"qwen profile %q carries unknown secret %q",
				profile.ID,
				name,
			)
		}
	}
	if secret, ok := profile.Secrets[SecretAPIKey]; ok {
		material.apiKey = strings.TrimSpace(string(secret.Bytes()))
	}
	if material.apiKey == "" {
		return profileMaterial{}, fmt.Errorf(
			"qwen profile %q needs the %q secret",
			profile.ID,
			SecretAPIKey,
		)
	}
	return material, nil
}

func (m profileMaterial) newClient(spec Spec) *dashClient {
	options := []httpkit.Option{
		httpkit.WithTimeout(10 * time.Minute),
		httpkit.WithResponseHeaderTimeout(5 * time.Minute),
	}
	if spec.HTTPRetries != nil {
		options = append(options, httpkit.WithRetryAttempts(*spec.HTTPRetries))
	}
	return &dashClient{
		http: httpkit.NewClient(options...),
		key:  m.apiKey,
		base: spec.apiBase(),
	}
}

// request posts one JSON payload; stream requests add the SSE header and
// leave the body open for scanning.
func (c *dashClient) request(
	ctx context.Context,
	path string,
	body any,
	stream bool,
) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("qwen: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.base+path, bytes.NewReader(raw),
	)
	if err != nil {
		return nil, fmt.Errorf("qwen: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("X-DashScope-SSE", "enable")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qwen: post %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		var failure struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		}
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		// A failure here means we cannot enrich classifyHTTPError with
		// the upstream code/message, but the snippet itself is still
		// passed through. Surface it so a malformed error body doesn't
		// go completely dark in logs.
		if uErr := json.Unmarshal(snippet, &failure); uErr != nil {
			telemetry.WarnErr(ctx, "qwen: parse error response body", uErr,
				otellog.String("provider", "qwen"),
				otellog.Int("http.status", resp.StatusCode))
		}
		classified := errdefs.WithRequestID(
			classifyHTTPError(
				resp.StatusCode, failure.Code, failure.Message, snippet,
			),
			failure.RequestID,
		)
		return nil, errdefs.WithRetryCount(
			errdefs.WithRetryAfter(
				classified,
				errdefs.ParseRetryAfter(resp.Header.Get("Retry-After")),
			),
			httpkit.RetryCountOf(resp),
		)
	}
	return resp, nil
}

func (c *dashClient) postJSON(
	ctx context.Context,
	path string,
	body, out any,
) error {
	resp, err := c.request(ctx, path, body, false)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("qwen: decode %s response: %w", path, err)
	}
	return nil
}

func (c *dashClient) postSSE(
	ctx context.Context,
	path string,
	body any,
) (io.ReadCloser, error) {
	resp, err := c.request(ctx, path, body, true)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
