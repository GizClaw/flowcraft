package minimax

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message/media"
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit"
)

// mediaClient speaks to MiniMax's media APIs (t2a, video, image) — plain
// JSON over HTTP with Bearer auth and a base_resp status envelope, no SDK.
// The Anthropic Messages surface rides anthropicgo instead (client.go).
type mediaClient struct {
	http *http.Client
	key  string
	base string // API root, e.g. https://api.minimaxi.com
}

func newMediaClient(key, base string) *mediaClient {
	return &mediaClient{
		http: &http.Client{
			Transport: httpkit.NewRetryTransport(nil, httpkit.DefaultRetry),
			Timeout:   10 * time.Minute,
		},
		key:  key,
		base: strings.TrimRight(base, "/"),
	}
}

func (c *mediaClient) request(
	ctx context.Context,
	method, path string,
	query url.Values,
	body any,
) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("minimax: marshal request: %w", err)
		}
		payload = bytes.NewReader(raw)
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, fmt.Errorf("minimax: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimax: %s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, classifyHTTPStatus(resp.StatusCode, fmt.Errorf(
			"minimax: %s %s: HTTP %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(snippet)),
		))
	}
	return resp, nil
}

// postJSON decodes a JSON response into out; the caller inspects the
// envelope's base_resp.
func (c *mediaClient) postJSON(
	ctx context.Context,
	path string,
	body, out any,
) error {
	resp, err := c.request(ctx, http.MethodPost, path, nil, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("minimax: decode %s response: %w", path, err)
	}
	return nil
}

func (c *mediaClient) getJSON(
	ctx context.Context,
	path string,
	query url.Values,
	out any,
) error {
	resp, err := c.request(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("minimax: decode %s response: %w", path, err)
	}
	return nil
}

// postSSE posts a streaming request and returns the event-stream body for
// the caller to scan; the caller closes it.
func (c *mediaClient) postSSE(
	ctx context.Context,
	path string,
	body any,
) (io.ReadCloser, error) {
	resp, err := c.request(ctx, http.MethodPost, path, nil, body)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// sseEvents scans an event-stream body for "data:" payloads, one JSON
// document per event. Blank lines and comment lines are skipped.
func sseEvents(body io.Reader, yield func([]byte) bool) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if !yield([]byte(payload)) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("minimax: read event stream: %w", err)
	}
	return nil
}

// hexDecode decodes one hex audio payload; MiniMax's t2a and music APIs
// deliver audio as hex strings.
func hexDecode(raw string) ([]byte, error) {
	data, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("minimax: audio payload is not valid hex: %w", err)
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Shared hex audio stream: t2a and music_generation stream the same SSE
// shape — {"data": {"audio": <hex>, "status": 1|2}} — so both drivers ride
// one scanner. Event order delivered to the runtime is: zero or more audio
// deltas, one finish event, then EOF.
// ---------------------------------------------------------------------------

// hexAudioStreamEvent is one SSE data payload: status 1 carries an audio
// chunk, status 2 terminates the stream (its audio may still carry the
// final chunk).
type hexAudioStreamEvent struct {
	Data struct {
		Audio  string `json:"audio"`
		Status int    `json:"status"`
	} `json:"data"`
	BaseResp baseResp `json:"base_resp"`
}

// hexAudioStreamRaw carries the negotiated format with every delta: the
// format is fixed at compile time, flows compile → transport → raw, and
// never passes through the stateless decoder's construction site.
type hexAudioStreamRaw struct {
	data   []byte
	format *media.AudioFormat
	last   bool
}

type hexAudioStream struct {
	body   io.ReadCloser
	events chan hexAudioStreamEvent
	errs   chan error
	format media.AudioFormat

	emitFinish bool // terminal chunk seen; finish event pending
	done       bool // finish delivered; next Next returns EOF
}

// newHexAudioStream takes ownership of an SSE body and starts scanning.
func newHexAudioStream(
	body io.ReadCloser,
	format media.AudioFormat,
) *hexAudioStream {
	stream := &hexAudioStream{
		body:   body,
		events: make(chan hexAudioStreamEvent, 16),
		errs:   make(chan error, 1),
		format: format,
	}
	go stream.pump()
	return stream
}

// pump scans SSE payloads off the body and feeds parsed events.
func (s *hexAudioStream) pump() {
	defer close(s.events)
	err := sseEvents(s.body, func(payload []byte) bool {
		var event hexAudioStreamEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			s.errs <- fmt.Errorf("minimax: audio stream event: %w", err)
			return false
		}
		s.events <- event
		return true
	})
	if err != nil {
		s.errs <- err
	}
}

func (s *hexAudioStream) Next(ctx context.Context) (hexAudioStreamRaw, error) {
	if s.emitFinish {
		s.emitFinish = false
		s.done = true
		return hexAudioStreamRaw{last: true}, nil
	}
	if s.done {
		return hexAudioStreamRaw{}, io.EOF
	}
	select {
	case <-ctx.Done():
		return hexAudioStreamRaw{}, ctx.Err()
	case failure := <-s.errs:
		return hexAudioStreamRaw{}, failure
	case event, open := <-s.events:
		if !open {
			select {
			case failure := <-s.errs:
				return hexAudioStreamRaw{}, failure
			default:
			}
			return hexAudioStreamRaw{}, fmt.Errorf(
				"minimax: audio stream ended without a terminal chunk",
			)
		}
		if failure := event.BaseResp.err("audio stream"); failure != nil {
			return hexAudioStreamRaw{}, failure
		}
		if event.Data.Status == 2 {
			s.emitFinish = true
		}
		if event.Data.Audio == "" {
			if s.emitFinish {
				// Silent terminal chunk: finish immediately.
				s.emitFinish = false
				s.done = true
				return hexAudioStreamRaw{last: true}, nil
			}
			return s.Next(ctx) // progress-only event
		}
		data, err := hexDecode(event.Data.Audio)
		if err != nil {
			return hexAudioStreamRaw{}, err
		}
		format := s.format
		return hexAudioStreamRaw{data: data, format: &format}, nil
	}
}

func (s *hexAudioStream) Close() error {
	return s.body.Close()
}

// decodeHexAudioStream turns chunks into audio deltas. It is pure: the
// negotiated format arrives on every raw delta, and the runtime accepts
// repeated identical formats.
func decodeHexAudioStream(
	_ context.Context,
	raw hexAudioStreamRaw,
) (inference.GenerateStreamEvent, error) {
	if raw.last {
		return inference.GenerateStreamEvent{
			FinishReason: inference.FinishCompleted,
		}, nil
	}
	if len(raw.data) == 0 || raw.format == nil {
		return inference.GenerateStreamEvent{}, fmt.Errorf(
			"minimax: audio chunk carried no data",
		)
	}
	format := *raw.format
	return inference.GenerateStreamEvent{
		Delta: inference.AudioPartDelta{Data: raw.data, Format: &format},
	}, nil
}
