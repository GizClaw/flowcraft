package kimi

import (
	"bufio"
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
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit"
)

const defaultBaseURL = "https://api.moonshot.cn/v1"

// profileMaterial is one profile's resolved credentials and profile-level
// settings, validated once at factory build time.
type profileMaterial struct {
	spec   ProfileSpec
	apiKey string
}

func newProfileMaterial(profile config.ResolvedProfile) (profileMaterial, error) {
	spec, err := decodeProfileSpec(profile.Spec)
	if err != nil {
		return profileMaterial{}, fmt.Errorf("profile %q: %w", profile.ID, err)
	}
	material := profileMaterial{spec: spec}
	for name, secret := range profile.Secrets {
		switch name {
		case SecretAPIKey:
			material.apiKey = secretString(secret)
		default:
			return profileMaterial{}, fmt.Errorf("profile %q carries unknown secret %q", profile.ID, name)
		}
	}
	if material.apiKey == "" {
		return profileMaterial{}, fmt.Errorf("profile %q resolves no api_key secret", profile.ID)
	}
	return material, nil
}

func secretString(secret config.Secret) string {
	return strings.TrimSpace(string(secret.Bytes()))
}

func (m profileMaterial) newClient(spec Spec) *kimiClient {
	baseURL := strings.TrimRight(spec.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &kimiClient{
		baseURL: baseURL,
		apiKey:  m.apiKey,
		http: httpkit.NewClient(
			httpkit.WithTimeout(300*time.Second),
			httpkit.WithResponseHeaderTimeout(5*time.Minute),
		),
	}
}

// kimiClient renders requests itself: Kimi owns fields the openai-go SDK
// does not model (thinking, reasoning_effort, video_url content parts,
// prompt_cache_key), so a plain JSON-over-HTTP client is the honest
// transport.
type kimiClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func (c *kimiClient) newRequest(ctx context.Context, body any, stream bool) (*http.Request, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("kimi: encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("kimi: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	return request, nil
}

// postJSON executes one unary request and decodes the response body.
// Non-2xx statuses classify from the error envelope.
func (c *kimiClient) postJSON(ctx context.Context, body any, out any) error {
	request, err := c.newRequest(ctx, body, false)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return classifyError(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return errdefs.NotAvailable(fmt.Errorf("kimi: read response: %w", err))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return classifyHTTPError(ctx, response.StatusCode, payload)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return errdefs.NotAvailable(fmt.Errorf("kimi: decode response: %w", err))
	}
	return nil
}

// sseEvent is one raw server-sent event.
type sseEvent struct {
	data []byte
}

// sseEvents streams the data: lines of one SSE response body until the
// [DONE] sentinel or EOF. The scanner buffer is raised past the default
// 64KiB because usage-carrying chunks can be large.
func sseEvents(ctx context.Context, body io.Reader) chan sseEvent {
	events := make(chan sseEvent, 16)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) {
				return
			}
			if len(data) == 0 {
				continue
			}
			event := sseEvent{data: bytes.Clone(data)}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events
}
