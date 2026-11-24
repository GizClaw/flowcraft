package dashscope

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/outcome"
)

// streamFrame renders one wire frame as the gateway sends it: the answer nested
// under output.choices, with the usage on the terminal one.
func streamFrame(choice string) string {
	return `{"request_id":"req-1","output":{"choices":[` + choice + `]}}`
}

// The decoder turns wire fragments into contract events, and its two rules are
// what a consumer depends on: the stream starts exactly once, and the usage
// arrives before the end so a caller can price the answer before it stops.
func TestGenerateStreamDecoder(t *testing.T) {
	decode := newGenerateStreamDecoder(inference.GenerateRequest{})
	ctx := context.Background()

	events, err := decode(ctx, generateStreamRaw{kind: generateStreamReasoning, part: 1, text: "thinking"})
	if err != nil {
		t.Fatalf("reasoning: %v", err)
	}
	if len(events) != 2 || events[0].Kind != inference.StreamStart ||
		events[1].Kind != inference.StreamReasoning || events[1].Text != "thinking" ||
		events[1].Index != 1 {
		t.Fatalf("first fragment = %+v", events)
	}

	// A second fragment does not repeat the start.
	events, err = decode(ctx, generateStreamRaw{kind: generateStreamText, part: 0, text: "hello"})
	if err != nil {
		t.Fatalf("text: %v", err)
	}
	if len(events) != 1 || events[0].Kind != inference.StreamText {
		t.Fatalf("text fragment = %+v", events)
	}

	events, err = decode(ctx, generateStreamRaw{
		kind: generateStreamTool, part: 2,
		toolID: "call-1", toolName: "search", toolArgs: `{"q":"x"}`,
	})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	if len(events) != 1 || events[0].Kind != inference.StreamToolCall ||
		events[0].Tool == nil || events[0].Tool.Name != "search" ||
		events[0].Tool.ID != "call-1" || events[0].Tool.Arguments != `{"q":"x"}` {
		t.Fatalf("tool fragment = %+v", events)
	}

	accounting := outcome.Usage{InputTokens: 5, OutputTokens: 7}
	events, err = decode(ctx, generateStreamRaw{
		kind: generateStreamFinish, finish: inference.FinishToolCalls, usage: &accounting,
	})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(events) != 2 || events[0].Kind != inference.StreamUsage ||
		events[0].Usage == nil || events[0].Usage.OutputTokens != 7 ||
		events[1].Kind != inference.StreamEnd || events[1].Finish != inference.FinishToolCalls {
		t.Fatalf("finish fragment = %+v", events)
	}

	// A finish without usage still ends the stream.
	events, err = decode(ctx, generateStreamRaw{kind: generateStreamFinish, finish: inference.FinishStop})
	if err != nil {
		t.Fatalf("finish without usage: %v", err)
	}
	if len(events) != 1 || events[0].Kind != inference.StreamEnd {
		t.Fatalf("bare finish = %+v", events)
	}

	if _, err := decode(ctx, generateStreamRaw{kind: generateStreamKind(99)}); err == nil {
		t.Error("an unknown fragment kind was accepted")
	}
}

// The buffered stream is what the opener hands back: the fragments in order, then
// io.EOF, and a Close that releases nothing because the body is already read.
func TestBufferedGenerateStream(t *testing.T) {
	stream := &bufferedGenerateStream{raws: []generateStreamRaw{
		{kind: generateStreamText, text: "a"},
		{kind: generateStreamFinish},
	}}
	ctx := context.Background()
	for i, want := range []string{"a", ""} {
		raw, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("fragment %d: %v", i, err)
		}
		if raw.text != want {
			t.Errorf("fragment %d = %q, want %q", i, raw.text, want)
		}
	}
	if _, err := stream.Next(ctx); !errors.Is(err, io.EOF) {
		t.Errorf("end of stream = %v, want io.EOF", err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// streamServer answers the generate path with an SSE body, and with the JSON form
// on the path that asks for one.
func streamServer(t *testing.T, body string, contentType string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}))
	// A helper cannot defer: the server has to outlive this function and be
	// closed with the test.
	t.Cleanup(server.Close)
	return server
}

func streamWire(path string) generateWire {
	return generateWire{path: path}
}

// The opener is where the wire becomes fragments: reasoning and content are
// separate parts, tool calls are indexed by the platform's own index, and the
// finish carries the usage.
func TestOpenGenerateStream(t *testing.T) {
	body := strings.Join([]string{
		"data: " + streamFrame(`{"message":{"reasoning_content":"hmm"}}`),
		"",
		"data: " + streamFrame(`{"message":{"content":"hello"}}`),
		"",
		"data: " + streamFrame(`{"message":{"content":[{"text":" world"}]}}`),
		"",
		"data: " + streamFrame(`{"message":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"search","arguments":"{}"}}]}}`),
		"",
		// Two frames, not one multi-line payload: the usage rides its own frame
		// here so the reader keeps them separate.
		"data: " + streamFrame(`{}`),
		"",
		`data: {"request_id":"req-1","output":{"choices":[{"finish_reason":"stop"}]},"usage":{"input_tokens":3,"output_tokens":4}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	server := streamServer(t, body, "text/event-stream")
	client := newMediaClient("key", server.URL, "", Spec{})

	stream, err := openGenerateStream(client)(context.Background(), inference.GenerateRequest{},
		streamWire("/generation"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var raws []generateStreamRaw
	for {
		raw, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		raws = append(raws, raw)
	}
	// The sequence is the contract: reasoning and content are separate parts,
	// both content frames share the text part, tool calls keep the platform's own
	// index, and the finish carries the usage. An exact list means a fragment the
	// opener stops producing fails here instead of quietly shrinking the stream.
	type fragment struct {
		kind generateStreamKind
		part int
		text string
		tool string
		args string
	}
	want := []fragment{
		{kind: generateStreamReasoning, part: 0, text: "hmm"},
		{kind: generateStreamText, part: 1, text: "hello"},
		{kind: generateStreamText, part: 1, text: " world"},
		{kind: generateStreamTool, part: 2, tool: "search", args: "{}"},
		{kind: generateStreamFinish},
	}
	var got []fragment
	for _, raw := range raws {
		got = append(got, fragment{
			kind: raw.kind, part: raw.part, text: raw.text,
			tool: raw.toolName, args: raw.toolArgs,
		})
	}
	if len(got) != len(want) {
		t.Fatalf("fragments = %d, want %d: %+v", len(got), len(want), got)
	}
	for index := range want {
		if got[index].kind != want[index].kind || got[index].part != want[index].part ||
			got[index].text != want[index].text ||
			got[index].tool != want[index].tool || got[index].args != want[index].args {
			t.Errorf("fragment %d = %+v, want %+v", index, got[index], want[index])
		}
	}
	var finish *generateStreamRaw
	for index := range raws {
		if raws[index].kind == generateStreamFinish {
			finish = &raws[index]
		}
	}
	if finish == nil {
		t.Fatal("no finish fragment")
	}
	if finish.usage == nil || finish.usage.InputTokens != 3 || finish.usage.OutputTokens != 4 {
		t.Errorf("finish usage = %+v", finish.usage)
	}
	if finish.finish != inference.FinishStop {
		t.Errorf("finish reason = %v", finish.finish)
	}

	// A plain JSON answer is a one-frame stream: the gateway uses the same body
	// for a non-streaming call.
	jsonServer := streamServer(t, streamFrame(`{"message":{"content":"whole"}}`), "application/json")
	jsonStream, err := openGenerateStream(newMediaClient("key", jsonServer.URL, "", Spec{}))(
		context.Background(), inference.GenerateRequest{}, streamWire("/generation"))
	if err != nil {
		t.Fatalf("json open: %v", err)
	}
	raw, err := jsonStream.Next(context.Background())
	if err != nil || raw.text != "whole" {
		t.Fatalf("json fragment = %+v (%v)", raw, err)
	}
}

// A stream that fails mid-flight carries the envelope the platform uses for
// everything else, so the same classification applies and the caller sees why.
func TestOpenGenerateStreamReportsAFrameFailure(t *testing.T) {
	body := "data: " + `{"request_id":"req-9","code":"Throttling.RateQuota","message":"slow down"}` + "\n\n"
	server := streamServer(t, body, "text/event-stream")
	_, err := openGenerateStream(newMediaClient("key", server.URL, "", Spec{}))(
		context.Background(), inference.GenerateRequest{}, streamWire("/generation"))
	if err == nil {
		t.Fatal("a failing frame was accepted")
	}
	if !errdefs.IsRateLimit(err) {
		t.Errorf("failure = %v, want RateLimit", err)
	}
	if id, ok := errdefs.RequestID(err); !ok || id != "req-9" {
		t.Errorf("request id = %q (%v), want req-9", id, ok)
	}

	// A frame that is not JSON at all is a protocol failure, named as one.
	broken := streamServer(t, "data: {not json}\n\n", "text/event-stream")
	if _, err := openGenerateStream(newMediaClient("key", broken.URL, "", Spec{}))(
		context.Background(), inference.GenerateRequest{}, streamWire("/generation")); err == nil {
		t.Error("a malformed frame was accepted")
	}
}

// postSSEEvents is the reader under both surfaces: an SSE body yields its
// payloads without the envelope, `[DONE]` is dropped, and an empty body is a
// failure rather than an empty answer.
func TestPostSSEEvents(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  int
		isErr bool
	}{
		{name: "sse", body: "data: {\"a\":1}\n\ndata: [DONE]\n\ndata: {\"b\":2}\n\n", want: 2},
		{name: "json", body: `{"a":1}`, want: 1},
		{name: "empty", body: "  \n", isErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := streamServer(t, test.body, "text/event-stream")
			payloads, err := newMediaClient("key", server.URL, "", Spec{}).postSSEEvents(
				context.Background(), "/generation", map[string]any{"model": "m"}, true)
			if test.isErr {
				if err == nil {
					t.Fatal("an empty body was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			if len(payloads) != test.want {
				t.Fatalf("payloads = %d, want %d", len(payloads), test.want)
			}
			for _, payload := range payloads {
				if strings.Contains(string(payload), "[DONE]") {
					t.Errorf("payload kept the terminator: %q", payload)
				}
			}
		})
	}
}

// getArtifact fetches a signed link with no credential: the link carries its own
// signature, and sending the account key to the artifact host would spread it for
// nothing.
func TestGetArtifact(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("result-bytes"))
	}))
	defer server.Close()
	client := newMediaClient("secret", server.URL, "", Spec{})
	body, err := client.getArtifact(context.Background(), server.URL+"/signed/result")
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	if string(body) != "result-bytes" {
		t.Errorf("body = %q", body)
	}
	if gotAuth != "" {
		t.Errorf("authorization = %q, want the artifact fetched without the account key", gotAuth)
	}
	// A link that is not an http(s) URL never reaches the network: the terminal
	// task query can name a relative path, and downloading it would be nonsense.
	for _, link := range []string{"/local/result", "file:///etc/passwd", "result.json"} {
		if _, err := client.getArtifact(context.Background(), link); err == nil {
			t.Errorf("link %q was fetched", link)
		}
	}
}

// The poll interval is the deployment's when it sets one, and the default
// otherwise; polling faster than a task surface changes only spends rate limit.
func TestPollInterval(t *testing.T) {
	if got := newMediaClient("k", "http://example.test", "", Spec{}).pollInterval(); got != defaultPollInterval {
		t.Errorf("default interval = %s, want %s", got, defaultPollInterval)
	}
	spec := Spec{PollIntervalMillis: int((2 * defaultPollInterval).Milliseconds())}
	if got := newMediaClient("k", "http://example.test", "", spec).pollInterval(); got != 2*defaultPollInterval {
		t.Errorf("configured interval = %s, want %s", got, 2*defaultPollInterval)
	}
}
