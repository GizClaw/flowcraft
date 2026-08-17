package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/inference"
)

// wireStreamEvent is one SSE event forwarded from the plugin. The
// delta payload is the marshaled canonical PartDelta: a text delta is
// {"text": ...}, a tool call delta carries arguments_fragment, a
// reasoning delta carries text/signature, audio carries data, and an
// image carries part.
type wireStreamEvent struct {
	PartIndex       int                       `json:"part_index,omitempty"`
	Delta           json.RawMessage           `json:"delta,omitempty"`
	Usage           *inference.Usage          `json:"usage,omitempty"`
	FinishReason    inference.FinishReason    `json:"finish_reason,omitempty"`
	ProviderOutputs inference.ProviderOutputs `json:"provider_outputs,omitempty"`
	RequestID       string                    `json:"request_id,omitempty"`
	ResponseID      string                    `json:"response_id,omitempty"`
}

type streamRequest struct {
	Handle string          `json:"handle"`
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

// decodeStreamEvent implements inference.GenerateStreamDecoder: it
// translates one plugin event into the canonical event.
func decodeStreamEvent(
	ctx context.Context,
	event wireStreamEvent,
) (inference.GenerateStreamEvent, error) {
	decoded := inference.GenerateStreamEvent{
		PartIndex:       event.PartIndex,
		Usage:           event.Usage,
		FinishReason:    event.FinishReason,
		ProviderOutputs: event.ProviderOutputs,
		RequestID:       event.RequestID,
		ResponseID:      event.ResponseID,
	}
	if len(event.Delta) == 0 ||
		bytes.Equal(bytes.TrimSpace(event.Delta), []byte("null")) {
		// Terminal events carry no delta.
		if event.FinishReason == "" {
			return inference.GenerateStreamEvent{}, fmt.Errorf(
				"remote provider: stream event requires a delta or finish_reason")
		}
		return decoded, nil
	}
	delta, err := decodeDelta(event.Delta)
	if err != nil {
		return inference.GenerateStreamEvent{}, err
	}
	decoded.Delta = delta
	return decoded, nil
}

// decodeDelta decodes the delta payload by probing its fields. Probe
// order is image ("part"), audio ("data"), tool call
// ("arguments_fragment"), reasoning ("signature"), then text. A
// reasoning delta carrying only plain text decodes as text; plugins
// that need a text-only reasoning delta should include a signature or
// id.
func decodeDelta(raw json.RawMessage) (inference.PartDelta, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("remote provider: stream delta is required")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("remote provider: stream delta: %w", err)
	}
	switch {
	case fields["part"] != nil:
		var delta inference.ImagePartDelta
		if err := json.Unmarshal(raw, &delta); err != nil {
			return nil, fmt.Errorf("remote provider: image delta: %w", err)
		}
		return delta, nil
	case fields["data"] != nil:
		var delta inference.AudioPartDelta
		if err := json.Unmarshal(raw, &delta); err != nil {
			return nil, fmt.Errorf("remote provider: audio delta: %w", err)
		}
		return delta, nil
	case fields["arguments_fragment"] != nil:
		var delta inference.ToolCallDelta
		if err := json.Unmarshal(raw, &delta); err != nil {
			return nil, fmt.Errorf("remote provider: tool call delta: %w", err)
		}
		return delta, nil
	case fields["signature"] != nil:
		var delta inference.ReasoningDelta
		if err := json.Unmarshal(raw, &delta); err != nil {
			return nil, fmt.Errorf("remote provider: reasoning delta: %w", err)
		}
		return delta, nil
	default:
		var delta inference.TextPartDelta
		if err := json.Unmarshal(raw, &delta); err != nil {
			return nil, fmt.Errorf("remote provider: text delta: %w", err)
		}
		return delta, nil
	}
}

// openSSE opens the plugin's /stream endpoint and returns a provider
// event stream.
func openSSE(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	headers map[string]string,
	handle, method string,
	args []byte,
) (*sseEventStream, error) {
	payload, err := json.Marshal(streamRequest{
		Handle: handle,
		Method: method,
		Args:   json.RawMessage(args),
	})
	if err != nil {
		return nil, fmt.Errorf("remote provider: encode stream request: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/stream",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("remote provider: stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf(
			"remote provider: stream endpoint returned %s", resp.Status)
	}
	return newSSEEventStream(resp.Body), nil
}

// sseEventStream is an inference.ProviderStream over server-sent
// events.
type sseEventStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	closed  chan struct{}
	once    sync.Once
}

func newSSEEventStream(body io.ReadCloser) *sseEventStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	return &sseEventStream{
		body:    body,
		scanner: scanner,
		closed:  make(chan struct{}),
	}
}

// Next implements inference.ProviderStream.
func (s *sseEventStream) Next(ctx context.Context) (wireStreamEvent, error) {
	type result struct {
		event wireStreamEvent
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		event, err := s.nextBlocking()
		ch <- result{event: event, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = s.Close()
		return wireStreamEvent{}, ctx.Err()
	case res := <-ch:
		return res.event, res.err
	}
}

// Close implements inference.ProviderStream.
func (s *sseEventStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.body.Close()
}

func (s *sseEventStream) nextBlocking() (wireStreamEvent, error) {
	var dataLines []string
	for s.scanner.Scan() {
		line := s.scanner.Text()
		switch {
		case line == "":
			if len(dataLines) > 0 {
				return decodeSSEEvent(dataLines)
			}
		case strings.HasPrefix(line, "data:"):
			dataLines = append(
				dataLines,
				strings.TrimSpace(strings.TrimPrefix(line, "data:")),
			)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return wireStreamEvent{}, err
	}
	if len(dataLines) > 0 {
		return decodeSSEEvent(dataLines)
	}
	return wireStreamEvent{}, io.EOF
}

func decodeSSEEvent(dataLines []string) (wireStreamEvent, error) {
	var event wireStreamEvent
	data := strings.Join(dataLines, "\n")
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return wireStreamEvent{}, fmt.Errorf(
			"remote provider: decode SSE event: %w", err)
	}
	return event, nil
}
