package dashscope

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/resource"
)

// The live lanes drive the real driver against the real DashScope endpoint.
// Everything else in this package asserts what the platform documents; these
// lanes assert what it does.
//
// A media call is a purchase, so the lanes are gated twice: a key has to
// resolve, and the lane has to be opted into on top of that. An .env with a
// key in it must not turn `make test` into a bill.
//
//	DASHSCOPE_API_KEY       environment or repo-root .env; unset => every lane skips
//	DASHSCOPE_LIVE=1        the lanes that generate an image
//	DASHSCOPE_LIVE_VIDEO=1  the lanes that generate a video (slow as well as paid)
//	DASHSCOPE_LIVE_KEYFRAMES=1  the keyframe lane, stacked on the video opt-in;
//	                            it cannot be probed cheaply
//	DASHSCOPE_LIVE_WAN3=1   the Wan 3.0 media-array lanes, stacked on the video
//	                        opt-in; the array's create call is the purchase
//	DASHSCOPE_LIVE_TEXT=1   the text-generation and embedding lanes: one
//	                        short generation, one streamed generation, and
//	                        one embedding batch
//	DASHSCOPE_LIVE_TTS=1    the speech-synthesis lane: one short synthesis,
//	                        a fraction of a cent at the platform's rate
//	DASHSCOPE_LIVE_ASR=1    the file-transcription lane: one recognition over
//	                        a platform sample URL, a fraction of a cent
//	DASHSCOPE_BASE_URL      host override; defaults to the Beijing host
//	DASHSCOPE_LIVE_DUMP     directory to keep the recorded transcripts in
//
// The lanes run the provider the way a deployment assembles it (the public
// resource factory) but root it at a recording proxy in front of the real
// host. The endpoint that answers is still the real one, so an accepted call
// proves the wire shape is right, and the recording proves the driver sent the
// shape its compile report said it would — the two halves of "correct" that a
// smoke test alone cannot separate.
const (
	envLiveKey    = "DASHSCOPE_API_KEY"
	envLiveImage  = "DASHSCOPE_LIVE"
	envLiveVideo  = "DASHSCOPE_LIVE_VIDEO"
	envLiveFrames = "DASHSCOPE_LIVE_KEYFRAMES"
	envLiveWan3   = "DASHSCOPE_LIVE_WAN3"
	envLiveText   = "DASHSCOPE_LIVE_TEXT"
	envLiveTTS    = "DASHSCOPE_LIVE_TTS"
	envLiveASR    = "DASHSCOPE_LIVE_ASR"
	envLiveBase   = "DASHSCOPE_BASE_URL"
	envLiveDump   = "DASHSCOPE_LIVE_DUMP"

	// liveProviderID is the settings id the lanes' provider builds under; it
	// is unrelated to providerID, which names the module's own default.
	liveProviderID = "alibaba-live"
	// liveProfileID is the single credential profile the lanes declare.
	liveProfileID = "live"
	// livePollMillis paces the task poll fast enough to keep a lane short.
	livePollMillis = 2000
)

// liveConfig is one lane's resolved credentials and host.
type liveConfig struct {
	key  string
	base string
}

// liveKeyOnly gates the lanes that cannot generate anything: the platform
// answers them with an error, so holding a key is the whole cost.
func liveKeyOnly(t *testing.T) liveConfig {
	t.Helper()
	config := liveConfig{
		key:  liveValue(envLiveKey),
		base: nonEmpty(liveValue(envLiveBase), DefaultBaseURL),
	}
	if config.key == "" {
		t.Skipf("live lane: %s is unset (environment or repo-root .env)", envLiveKey)
	}
	return config
}

// liveSpeech gates the lane that synthesizes audio. One short sentence is a
// fraction of a cent, but it is still a call: the lane asks to be opted into
// like the text one.
func liveSpeech(t *testing.T) liveConfig {
	t.Helper()
	config := liveKeyOnly(t)
	if liveValue(envLiveTTS) == "" {
		t.Skipf("live lane: %s is unset; skipping the paid call", envLiveTTS)
	}
	return config
}

// liveTranscripts gates the lane that recognizes an audio file. One short
// recognition is a fraction of a cent, but it is still a call, so the lane
// asks to be opted into like the text and speech ones.
func liveTranscripts(t *testing.T) liveConfig {
	t.Helper()
	config := liveKeyOnly(t)
	if liveValue(envLiveASR) == "" {
		t.Skipf("live lane: %s is unset; skipping the paid call", envLiveASR)
	}
	return config
}

// liveImages gates the lanes that produce an image.
func liveImages(t *testing.T) liveConfig {
	t.Helper()
	config := liveKeyOnly(t)
	if liveValue(envLiveImage) == "" {
		t.Skipf("live lane: %s is unset; skipping the paid call", envLiveImage)
	}
	return config
}

// liveTexts gates the lanes that call the text surfaces. A short generation
// and one embedding batch are the cheapest calls this module can make, but
// they are still calls, so they ask to be opted into like every other
// credentialed lane.
func liveTexts(t *testing.T) liveConfig {
	t.Helper()
	config := liveKeyOnly(t)
	if liveValue(envLiveText) == "" {
		t.Skipf("live lane: %s is unset; skipping the paid call", envLiveText)
	}
	return config
}

// liveVideos gates the lanes that produce a video.
func liveVideos(t *testing.T) liveConfig {
	t.Helper()
	config := liveKeyOnly(t)
	if liveValue(envLiveVideo) == "" {
		t.Skipf("live lane: %s is unset; skipping the paid video call", envLiveVideo)
	}
	return config
}

// liveKeyframes gates the keyframe lane on its own opt-in, stacked on the
// video opt-in. The keyframe surface has no cheap probe: the platform only
// cancels a task that is still PENDING, and it starts rendering within a
// second, so creating the task buys the render — the one to four yuan of a
// plus-tier clip.
func liveKeyframes(t *testing.T) liveConfig {
	t.Helper()
	config := liveVideos(t)
	if liveValue(envLiveFrames) == "" {
		t.Skipf("live lane: %s is unset; creating the task is paying for the clip",
			envLiveFrames)
	}
	return config
}

// liveWan3 gates the Wan 3.0 media lanes on their own opt-in, stacked on the
// video one. The surface has no cheap probe either: the platform validates the
// media array when the task starts rendering, so a create call that should have
// been refused is a clip that was bought.
func liveWan3(t *testing.T) liveConfig {
	t.Helper()
	config := liveVideos(t)
	if liveValue(envLiveWan3) == "" {
		t.Skipf("live lane: %s is unset; creating the task is paying for the clip",
			envLiveWan3)
	}
	return config
}

// liveValue resolves one setting: the process environment first, then the
// repository .env, which is git-ignored, so a key never has to reach a
// committed file.
func liveValue(name string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if values, err := readEnvFile(filepath.Join(dir, ".env")); err == nil {
			if value := values[name]; value != "" {
				return value
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// readEnvFile parses the KEY=VALUE subset of dotenv syntax these lanes need:
// comments, blank lines, and optional surrounding quotes.
func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if key = strings.TrimSpace(key); key != "" {
			values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return values, scanner.Err()
}

// ------------------------------------------------------------------ recording

// liveExchange is one recorded round trip: what the driver sent, and what the
// endpoint answered.
type liveExchange struct {
	Method   string
	Path     string
	Header   http.Header
	Body     []byte
	Status   int
	Response []byte
}

// liveRecorder keeps the exchanges one lane made, in order.
type liveRecorder struct {
	mu        sync.Mutex
	exchanges []liveExchange
}

func (r *liveRecorder) add(exchange liveExchange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exchanges = append(r.exchanges, exchange)
}

func (r *liveRecorder) all() []liveExchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]liveExchange(nil), r.exchanges...)
}

// newLiveProxy starts a recording reverse proxy in front of the real host. It
// streams responses through as they arrive (the interleaved surface answers
// with an event stream, and the lane has to see it stream, not arrive at
// once) while copying every byte it passes into the recorder.
func newLiveProxy(t *testing.T, upstreamBase string) (string, *liveRecorder) {
	t.Helper()
	upstream, err := url.Parse(upstreamBase)
	if err != nil {
		t.Fatalf("live proxy: upstream %q: %v", upstreamBase, err)
	}
	recorder := &liveRecorder{}
	client := &http.Client{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		exchange := liveExchange{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		}
		target := upstream.Scheme + "://" + upstream.Host + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		outbound, err := http.NewRequestWithContext(
			r.Context(), r.Method, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		copyHeader(outbound.Header, r.Header)
		// Ask for an identity response so the recording holds the platform's
		// own JSON rather than a compressed stream of it.
		outbound.Header.Del("Accept-Encoding")
		response, err := client.Do(outbound)
		if err != nil {
			exchange.Status = http.StatusBadGateway
			recorder.add(exchange)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = response.Body.Close() }()
		exchange.Status = response.StatusCode
		copyHeader(w.Header(), response.Header)
		// The body streams: length stays unknown so the driver sees each
		// event as it lands rather than the whole run at the end.
		w.Header().Del("Content-Length")
		w.WriteHeader(response.StatusCode)
		flusher, _ := w.(http.Flusher)
		var captured bytes.Buffer
		buffer := make([]byte, 4096)
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				captured.Write(buffer[:read])
				if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
					break
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if readErr != nil {
				break
			}
		}
		exchange.Response = captured.Bytes()
		recorder.add(exchange)
	}))
	t.Cleanup(server.Close)
	return server.URL, recorder
}

// copyHeader copies source onto target, leaving hop-by-hop headers out.
func copyHeader(target, source http.Header) {
	for name, values := range source {
		switch http.CanonicalHeaderKey(name) {
		case "Host", "Content-Length", "Connection", "Transfer-Encoding":
			continue
		}
		target[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}

// ------------------------------------------------------------------- harness

// liveHarness is the driver assembled the way a deployment assembles it,
// rooted at a recording proxy.
type liveHarness struct {
	t          *testing.T
	recorder   *liveRecorder
	definition inference.ProviderDefinition
}

func newLiveHarness(t *testing.T, config liveConfig) *liveHarness {
	t.Helper()
	base, recorder := newLiveProxy(t, config.base)
	rawSpec, err := json.Marshal(Spec{
		BaseURL:            base,
		PollIntervalMillis: resource.Int(livePollMillis),
		Models:             liveDeclarations(),
	})
	if err != nil {
		t.Fatalf("live harness: marshal spec: %v", err)
	}
	// The settings are built as plain JSON rather than by marshalling the
	// typed settings: resource.Secret redacts itself on the way out (by
	// design, so a credential never reaches a log or a snapshot), which would
	// send the literal string "<secret>" as the key.
	settings, err := json.Marshal(map[string]any{
		"id":   liveProviderID,
		"spec": json.RawMessage(rawSpec),
		"profiles": []any{map[string]any{
			"id":      liveProfileID,
			"secrets": map[string]any{SecretAPIKey: config.key},
		}},
	})
	if err != nil {
		t.Fatalf("live harness: marshal settings: %v", err)
	}
	value, err := Factory().New(context.Background(), resource.NewInput(settings, nil, nil, nil))
	if err != nil {
		t.Fatalf("live harness: build provider: %v", err)
	}
	definition := value
	harness := &liveHarness{t: t, recorder: recorder, definition: definition}
	t.Cleanup(harness.dump)
	return harness
}

// openers returns the model implementation the built definition exposes for
// one declared model name.
func (h *liveHarness) openers(name string) inference.Openers {
	h.t.Helper()
	id := model.ModelID{Provider: liveProviderID, Name: name}
	for _, implementation := range h.definition.Models {
		if implementation.Descriptor.ID == id {
			return implementation.Openers
		}
	}
	h.t.Fatalf("live harness: %s is not part of the definition", name)
	return inference.Openers{}
}

// generate runs one unary generate call through the assembled driver: compile,
// then execute against the real endpoint.
func (h *liveHarness) generate(
	ctx context.Context,
	name string,
	request inference.GenerateRequest,
) (inference.GenerateResponse, inference.Explanation) {
	h.t.Helper()
	reference := model.ModelRef{
		ID:      model.ModelID{Provider: liveProviderID, Name: name},
		Profile: liveProfileID,
	}
	openers := h.openers(name)
	if openers.Generate == nil {
		h.t.Fatalf("live harness: %s declares no generate openers", name)
	}
	operations, err := openers.Generate(ctx, reference)
	if err != nil {
		h.t.Fatalf("live harness: open %s: %v", name, err)
	}
	prepared, err := operations.Unary.Prepare(ctx, reference, request)
	if err != nil {
		h.t.Fatalf("live harness: prepare %s: %v", name, err)
	}
	explanation := prepared.Explanation()
	response, err := prepared.Execute(ctx)
	if err != nil {
		h.t.Fatalf("live harness: execute %s: %v; last answer: %s",
			name, err, truncate(h.lastResponse()))
	}
	return response, explanation
}

// embed runs one embedding call through the assembled driver.
func (h *liveHarness) embed(
	ctx context.Context,
	name string,
	request inference.EmbedRequest,
) inference.EmbedResponse {
	h.t.Helper()
	reference := model.ModelRef{
		ID:      model.ModelID{Provider: liveProviderID, Name: name},
		Profile: liveProfileID,
	}
	openers := h.openers(name)
	if openers.Embed == nil {
		h.t.Fatalf("live harness: %s declares no embed driver", name)
	}
	driver, err := openers.Embed(ctx, reference)
	if err != nil {
		h.t.Fatalf("live harness: open %s: %v", name, err)
	}
	prepared, err := driver.Prepare(ctx, reference, request)
	if err != nil {
		h.t.Fatalf("live harness: prepare %s: %v", name, err)
	}
	response, err := prepared.Execute(ctx)
	if err != nil {
		h.t.Fatalf("live harness: execute %s: %v; last answer: %s",
			name, err, truncate(h.lastResponse()))
	}
	return response
}

// transcribe runs one unary file-transcription call through the assembled
// driver: compile, then execute against the real endpoint.
func (h *liveHarness) transcribe(
	ctx context.Context,
	name string,
	request inference.TranscriptionRequest,
) (inference.TranscriptionResponse, inference.Explanation) {
	h.t.Helper()
	reference := model.ModelRef{
		ID:      model.ModelID{Provider: liveProviderID, Name: name},
		Profile: liveProfileID,
	}
	openers := h.openers(name)
	if openers.Transcribe == nil {
		h.t.Fatalf("live harness: %s declares no transcribe openers", name)
	}
	operations, err := openers.Transcribe(ctx, reference)
	if err != nil {
		h.t.Fatalf("live harness: open %s: %v", name, err)
	}
	if operations.Unary == nil {
		h.t.Fatalf("live harness: %s declares no unary transcription driver", name)
	}
	prepared, err := operations.Unary.Prepare(ctx, reference, request)
	if err != nil {
		h.t.Fatalf("live harness: prepare %s: %v", name, err)
	}
	explanation := prepared.Explanation()
	response, err := prepared.Execute(ctx)
	if err != nil {
		h.t.Fatalf("live harness: execute %s: %v; last answer: %s",
			name, err, truncate(h.lastResponse()))
	}
	return response, explanation
}

// stream drains one streaming generate call through the assembled driver and
// returns the accumulated result plus how many text deltas arrived, so a lane
// can tell an endpoint that streamed from one that answered in one frame.
func (h *liveHarness) stream(
	ctx context.Context,
	name string,
	request inference.GenerateRequest,
) (inference.GenerateResponse, inference.Explanation, int) {
	h.t.Helper()
	reference := model.ModelRef{
		ID:      model.ModelID{Provider: liveProviderID, Name: name},
		Profile: liveProfileID,
	}
	openers := h.openers(name)
	if openers.Generate == nil {
		h.t.Fatalf("live harness: %s declares no generate openers", name)
	}
	operations, err := openers.Generate(ctx, reference)
	if err != nil {
		h.t.Fatalf("live harness: open %s: %v", name, err)
	}
	if operations.Stream == nil {
		h.t.Fatalf("live harness: %s declares no stream driver", name)
	}
	prepared, err := operations.Stream.PrepareStream(ctx, reference, request)
	if err != nil {
		h.t.Fatalf("live harness: prepare stream %s: %v", name, err)
	}
	explanation := prepared.Explanation()
	stream, err := prepared.Execute(ctx)
	if err != nil {
		h.t.Fatalf("live harness: open stream %s: %v; last answer: %s",
			name, err, truncate(h.lastResponse()))
	}
	defer func() {
		if err := stream.Close(); err != nil {
			h.t.Errorf("live harness: close stream %s: %v", name, err)
		}
	}()
	deltas := 0
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			h.t.Fatalf("live harness: stream %s: %v; last answer: %s",
				name, err, truncate(h.lastResponse()))
		}
		switch event.Delta.(type) {
		case inference.TextPartDelta, *inference.TextPartDelta:
			deltas++
		}
	}
	response, err := stream.Result()
	if err != nil {
		h.t.Fatalf("live harness: stream result %s: %v", name, err)
	}
	return response, explanation, deltas
}

// lastResponse returns the platform's own bytes for the most recent call, so
// a failed lane reports what the endpoint said rather than only the class the
// driver derived from it.
func (h *liveHarness) lastResponse() []byte {
	exchanges := h.exchanges()
	if len(exchanges) == 0 {
		return nil
	}
	return exchanges[len(exchanges)-1].Response
}

// exchanges returns the calls this lane made.
func (h *liveHarness) exchanges() []liveExchange { return h.recorder.all() }

// first returns the first exchange whose path matches, the create call of a
// task lane.
func (h *liveHarness) first(path string) liveExchange {
	h.t.Helper()
	for _, exchange := range h.exchanges() {
		if exchange.Path == path {
			return exchange
		}
	}
	h.t.Fatalf("live lane: no exchange for %s; saw %s", path, h.paths())
	return liveExchange{}
}

func (h *liveHarness) paths() string {
	var seen []string
	for _, exchange := range h.exchanges() {
		seen = append(seen, exchange.Method+" "+exchange.Path)
	}
	return strings.Join(seen, ", ")
}

// dump keeps the lane's recorded transcript when asked for one. The
// Authorization header is redacted: a transcript is meant to become an offline
// fixture, and a fixture must never carry a credential.
func (h *liveHarness) dump() {
	directory := liveValue(envLiveDump)
	if directory == "" {
		return
	}
	type recorded struct {
		Method string      `json:"method"`
		Path   string      `json:"path"`
		Header http.Header `json:"header"`
		// The bodies are base64 so a recording stays byte-exact: the point of
		// keeping one is to promote it into an offline fixture, and a fixture
		// that lost its bytes would be a story about the wire, not the wire.
		Body     string `json:"body_base64,omitempty"`
		Status   int    `json:"status"`
		Response string `json:"response_base64,omitempty"`
	}
	exchanges := h.exchanges()
	if len(exchanges) == 0 {
		return
	}
	lines := make([]recorded, 0, len(exchanges))
	for _, exchange := range exchanges {
		header := exchange.Header.Clone()
		if _, ok := header["Authorization"]; ok {
			header["Authorization"] = []string{"Bearer <redacted>"}
		}
		lines = append(lines, recorded{
			Method:   exchange.Method,
			Path:     exchange.Path,
			Header:   header,
			Body:     base64.StdEncoding.EncodeToString(exchange.Body),
			Status:   exchange.Status,
			Response: base64.StdEncoding.EncodeToString(exchange.Response),
		})
	}
	raw, err := json.MarshalIndent(lines, "", "  ")
	if err != nil {
		h.t.Logf("live transcript: marshal: %v", err)
		return
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		h.t.Logf("live transcript: mkdir: %v", err)
		return
	}
	path := filepath.Join(directory, h.t.Name()+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		h.t.Logf("live transcript: write: %v", err)
		return
	}
	h.t.Logf("live transcript: %s (%d exchanges)", path, len(lines))
}

// ---------------------------------------------------------------- assertions

// liveImageOptionsField names an extension field the way the lanes' own
// declarations bind it: qualified by the deployment id. An option that does
// not name its deployment is inert rather than invalid, so a lane that used
// the module default would quietly assert nothing.
func liveImageOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(ImageOptions{Provider: liveProviderID})
}

// liveVideoOptionsField names a video-extension field the same way.
func liveVideoOptionsField(name string) inference.FieldID {
	return inference.ExtensionField(name).Qualify(VideoOptions{Provider: liveProviderID})
}

// liveBody decodes a recorded request body.
func liveBody(t *testing.T, exchange liveExchange) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(exchange.Body, &body); err != nil {
		t.Fatalf("live lane: recorded body %q: %v", exchange.Body, err)
	}
	return body
}

// liveNested walks a decoded body along a path of keys, descending into lists
// when a step is an index.
func liveNested(t *testing.T, body map[string]any, path ...string) any {
	t.Helper()
	var current any = body
	for index, key := range path {
		if list, ok := current.([]any); ok {
			position, err := strconv.Atoi(key)
			if err != nil || position < 0 || position >= len(list) {
				t.Fatalf("live lane: %s: %q is not an index of a %d-element list",
					strings.Join(path[:index], "."), key, len(list))
			}
			current = list[position]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("live lane: %s: %#v is neither an object nor a list",
				strings.Join(path[:index], "."), current)
		}
		current, ok = object[key]
		if !ok {
			t.Fatalf("live lane: recorded body carries no %s", strings.Join(path[:index+1], "."))
		}
	}
	return current
}

// liveHasKey reports whether a decoded body carries a key at any depth, which
// is how a lane checks a field did not ride the wire under another shape.
func liveHasKey(body any, key string) bool {
	switch value := body.(type) {
	case map[string]any:
		for name, nested := range value {
			if name == key || liveHasKey(nested, key) {
				return true
			}
		}
	case []any:
		for _, nested := range value {
			if liveHasKey(nested, key) {
				return true
			}
		}
	}
	return false
}

// liveHeader reads one request header off a recording.
func liveHeader(exchange liveExchange, name string) string {
	return exchange.Header.Get(name)
}

// liveDelivered asserts the compile report sent a field and the recorded body
// carries it: the ledger and the wire have to agree, or the report is a story
// about a request that was never made.
func liveDelivered(
	t *testing.T,
	explanation inference.Explanation,
	exchange liveExchange,
	field inference.FieldID,
	body map[string]any,
	key string,
) {
	t.Helper()
	decision, ok := decisionFor(explanationReport(t, explanation), field)
	if !ok {
		t.Fatalf("live lane: %s carries no decision", field)
	}
	if decision.Disposition != inference.Native {
		t.Fatalf("live lane: %s disposition = %s (%s), want native",
			field, decision.Disposition, decision.Reason)
	}
	if !liveHasKey(body, key) {
		t.Errorf("live lane: %s reported native, but %q is absent from %s",
			field, key, exchange.Body)
	}
}

// liveAbsent asserts a field the report did not send left no trace on the
// wire. A reported drop that still rode the body is the failure this driver
// exists to prevent: the caller would believe a fact was honored when the
// provider never saw it.
func liveAbsent(
	t *testing.T,
	explanation inference.Explanation,
	body map[string]any,
	field inference.FieldID,
	key string,
) {
	t.Helper()
	decision, ok := decisionFor(explanationReport(t, explanation), field)
	if !ok {
		t.Fatalf("live lane: %s carries no decision", field)
	}
	if decision.Disposition == inference.Native {
		t.Fatalf("live lane: %s reported native, want a drop or rejection", field)
	}
	if liveHasKey(body, key) {
		t.Errorf("live lane: %s was %s but %q still rode the body",
			field, decision.Disposition, key)
	}
}

// explanationReport adapts an explanation into the report shape the offline
// assertions use.
func explanationReport(t *testing.T, explanation inference.Explanation) inference.CompileReport {
	t.Helper()
	return inference.CompileReport{Decisions: explanation.Decisions}
}

// liveArtifact fetches a generated link and returns its leading bytes: a URL
// in a response is a claim, the bytes behind it are the evidence.
func liveArtifact(t *testing.T, link string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		t.Fatalf("live lane: artifact request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("live lane: fetch artifact: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("live lane: artifact %s: HTTP %d", link, response.StatusCode)
	}
	head, err := io.ReadAll(io.LimitReader(response.Body, 64))
	if err != nil {
		t.Fatalf("live lane: read artifact: %v", err)
	}
	if len(head) < 12 {
		t.Fatalf("live lane: artifact %s is %d bytes", link, len(head))
	}
	return head
}

// liveImageParts and liveVideoParts pull the generated links out of a
// response, so a lane can check both what the driver decoded and what the
// link actually serves.
func liveImageParts(t *testing.T, response inference.GenerateResponse) []string {
	t.Helper()
	var links []string
	for _, part := range response.Message.Content.Parts {
		if image, ok := part.(message.ImagePart); ok {
			links = append(links, image.Source.URL())
		}
	}
	if len(links) == 0 {
		t.Fatalf("live lane: response carries no image part: %#v", response.Message.Content.Parts)
	}
	return links
}

func liveVideoParts(t *testing.T, response inference.GenerateResponse) []string {
	t.Helper()
	var links []string
	for _, part := range response.Message.Content.Parts {
		if video, ok := part.(message.VideoPart); ok {
			links = append(links, video.Source.URL())
		}
	}
	if len(links) == 0 {
		t.Fatalf("live lane: response carries no video part: %#v", response.Message.Content.Parts)
	}
	return links
}

func liveText(t *testing.T, response inference.GenerateResponse) string {
	t.Helper()
	var builder strings.Builder
	for _, part := range response.Message.Content.Parts {
		if text, ok := part.(message.TextPart); ok {
			builder.WriteString(text.Text)
		}
	}
	return builder.String()
}

// liveGenerated returns the generated-asset count the response reported.
func liveGenerated(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}

// isPNG and isMP4 identify an artifact by its magic bytes rather than by what
// the platform said it would be.
func isPNG(head []byte) bool {
	return bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G'})
}

func isJPEG(head []byte) bool {
	return bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF})
}

func isMP4(head []byte) bool {
	return len(head) >= 8 && bytes.Equal(head[4:8], []byte("ftyp"))
}

// liveModels declares the line-up the live lanes exercise. A deployment
// declares what it serves; these are the declarations the lanes then hold the
// platform's own answers against.
// liveDeclarations is every model the lanes address: the media line-up plus
// the text and speech models the newer lanes declare.
func liveDeclarations() []ModelSpec {
	return append(
		append(append(liveModels(), liveTextModels()...), liveSpeechModels()...),
		liveTranscribeModels()...,
	)
}

// liveSpeechModels declares the model the speech lane addresses: the Qwen-TTS
// flash model, whose single WAV answer the lane reads back to check the
// declared payload shape against the platform's own bytes.
func liveSpeechModels() []ModelSpec {
	return []ModelSpec{
		{
			Name: "qwen3-tts-flash",
			Kind: "tts",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartText},
				Outputs: []message.PartKind{message.PartAudio},
			},
			TTS: TTSParams{
				PromptLimit:    600,
				LanguageSelect: true,
				Encoding:       media.AudioEncodingPCM16,
				SampleRateHz:   24000,
				Channels:       1,
			},
		},
	}
}

// liveTranscribeModels declares the model the transcription lane addresses:
// the asynchronous file pipeline's paraformer-v2, with every control the lane
// exercises declared.
func liveTranscribeModels() []ModelSpec {
	return []ModelSpec{
		{
			Name: "paraformer-v2",
			Kind: "transcribe",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartAudio},
				Outputs: []message.PartKind{message.PartText},
			},
			Transcribe: TranscribeParams{
				Context:       true,
				PromptLimit:   400,
				LanguageHints: true,
				Languages:     []string{"zh", "en"},
				Timestamps:    true,
			},
		},
	}
}

// liveTextModels declares the models the text lanes address: one generate
// model on the multimodal surface the Qwen3.5+ families require even for text
// input, and one embedding model whose declared batch cap of two makes a
// three-item request prove the batching rather than assume it.
func liveTextModels() []ModelSpec {
	return []ModelSpec{
		{
			Name: "qwen3.8-flash",
			Kind: "generate",
			Capabilities: model.ModelCapabilities{
				Inputs:    []message.PartKind{message.PartText, message.PartImage},
				Outputs:   []message.PartKind{message.PartText},
				Reasoning: model.ReasoningCapability{Kind: model.ReasoningToggle},
			},
			Generate: GenerateParams{API: generateAPIMultimodal},
		},
		{
			Name: "qwen3.7-text-embedding",
			Kind: "embed",
			Capabilities: model.ModelCapabilities{
				Inputs:                []message.PartKind{message.PartText},
				CustomEmbedDimensions: true,
			},
			Embed: EmbedParams{
				BatchMax:   2,
				Dimensions: []int{2560, 1024},
				Instruct:   true,
			},
		},
	}
}

func liveModels() []ModelSpec {
	return []ModelSpec{
		{
			Name: "wanx2.1-t2i-turbo",
			Kind: "image",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartText},
				Outputs: []message.PartKind{message.PartImage},
			},
			Image: ImageParams{
				PromptLimit: 500,
				MinEdge:     512,
				MaxEdge:     1440,
			},
		},
		{
			Name: "wan2.6-image",
			Kind: "image",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartText, message.PartImage},
				Outputs: []message.PartKind{message.PartImage, message.PartText},
			},
			Image: ImageParams{
				API:         imageAPISync,
				CountMax:    4,
				PromptLimit: 2000,
				MinPixels:   1280 * 1280,
				MaxPixels:   1440 * 1440,
			},
		},
		{
			Name: "wanx2.1-imageedit",
			Kind: "image",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartText, message.PartImage},
				Outputs: []message.PartKind{message.PartImage},
			},
			Image: ImageParams{
				Edit:        true,
				PromptLimit: 800,
				MinEdge:     512,
				MaxEdge:     4096,
			},
		},
		{
			Name: "wanx2.1-t2v-turbo",
			Kind: "video",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartText},
				Outputs: []message.PartKind{message.PartVideo},
			},
			Video: VideoParams{
				SizeParam:   videoSizeConcrete,
				Resolutions: []string{"480P", "720P"},
				Sizes: map[string]map[string]string{
					"480P": {"16:9": "832*480"},
					"720P": {"16:9": "1280*720"},
				},
				Durations:   []int{5},
				PromptLimit: 800,
			},
		},
		{
			Name: "wanx2.1-i2v-turbo",
			Kind: "video",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartText, message.PartImage},
				Outputs: []message.PartKind{message.PartVideo},
			},
			Video: VideoParams{
				Input:       videoInputImage,
				SizeParam:   videoSizeResolution,
				Resolutions: []string{"480P", "720P"},
				Durations:   []int{5},
				PromptLimit: 800,
			},
		},
		{
			Name: "wanx2.1-kf2v-plus",
			Kind: "video",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartText, message.PartImage},
				Outputs: []message.PartKind{message.PartVideo},
			},
			Video: VideoParams{
				Input:       videoInputKeyframes,
				SizeParam:   videoSizeResolution,
				Resolutions: []string{"720P"},
				Durations:   []int{5},
				PromptLimit: 800,
			},
		},
		{
			Name: "wan3.0-video",
			Kind: "video",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartText, message.PartImage},
				Outputs: []message.PartKind{message.PartVideo},
			},
			Video: VideoParams{
				Media:         &MediaParams{FirstFrame: true, LastFrame: true},
				Resolutions:   []string{"1080P", "720P", "480P"},
				MinDuration:   2,
				MaxDuration:   30,
				AudioSwitch:   true,
				SmartDuration: true,
				PromptLimit:   20000,
			},
		},
		{
			Name:      "wan3.0-video-refer",
			WireModel: "wan3.0-video",
			Kind:      "video",
			Capabilities: model.ModelCapabilities{
				Inputs:  []message.PartKind{message.PartText, message.PartImage},
				Outputs: []message.PartKind{message.PartVideo},
			},
			Video: VideoParams{
				Media: &MediaParams{
					ReferenceImages: 10,
					ReferenceVideos: 5,
					ReferenceAudios: 5,
					File:            true,
					Link:            true,
				},
				Resolutions:   []string{"1080P", "720P", "480P"},
				MinDuration:   2,
				MaxDuration:   30,
				AudioSwitch:   true,
				SmartDuration: true,
				PromptLimit:   20000,
			},
		},
	}
}

// ---------------------------------------------------------------------- lanes

// TestLiveUnknownTaskIsNotFound pins the platform behaviour the task poll
// depends on: a task id it does not know is not an HTTP error, it is an HTTP
// 200 whose status reads UNKNOWN. Reading that as "keep polling" would turn
// every expired task into a timeout instead of a not-found.
func TestLiveUnknownTaskIsNotFound(t *testing.T) {
	config := liveKeyOnly(t)
	client := newMediaClient(config.key, config.base, "", Spec{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	query, err := awaitTask(ctx, client, time.Second, "00000000-0000-0000-0000-000000000000", "live probe")
	if err == nil {
		t.Fatalf("live lane: unknown task resolved to %+v, want an error", query)
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("live lane: unknown task error = %v, want errdefs NotFound", err)
	}
	t.Logf("live lane: unknown task -> %v", err)
}

// TestLivePlatformRequiresAsyncHeader pins the fact that makes the async
// header mandatory: the task surfaces refuse a call that does not ask for a
// task. It cannot generate anything, so it costs nothing to run.
func TestLivePlatformRequiresAsyncHeader(t *testing.T) {
	config := liveKeyOnly(t)
	body := `{"model":"wanx2.1-t2i-turbo","input":{"prompt":"a red apple"},` +
		`"parameters":{"size":"1024*1024","n":1}}`
	status, response := liveRawPost(t, config, imagePathText2Image, nil, body)
	if status == http.StatusOK {
		t.Fatalf("live lane: the endpoint accepted a call without the async header: %s", response)
	}
	if !strings.Contains(strings.ToLower(string(response)), "synchronous") {
		t.Errorf("live lane: rejection = %s, want the synchronous-call refusal", response)
	}
	t.Logf("live lane: without %s -> HTTP %d %s", headerAsync, status, response)
}

// TestLiveInterleaveRequiresSSE pins the fact that makes the SSE header
// mandatory on the interleaved surface. It is a paid lane because the platform
// relaxing the rule would turn this probe into a real generation.
func TestLiveInterleaveRequiresSSE(t *testing.T) {
	config := liveImages(t)
	body := `{"model":"wan2.6-image","input":{"messages":[{"role":"user",` +
		`"content":[{"text":"a paper lantern"}]}]},` +
		`"parameters":{"n":1,"size":"1280*1280","enable_interleave":true}}`
	status, response := liveRawPost(t, config, imagePathMultimodal, nil, body)
	if status == http.StatusOK {
		t.Errorf("live lane: interleave was accepted without %s: %s", headerSSE, response)
		return
	}
	if !strings.Contains(strings.ToLower(string(response)), "stream") {
		t.Errorf("live lane: rejection = %s, want the stream requirement", response)
	}
	t.Logf("live lane: interleave without %s -> HTTP %d %s", headerSSE, status, response)
}

// liveRawPost sends one hand-built request at the real host, for the lanes
// that assert what the platform demands rather than what the driver sends.
func liveRawPost(
	t *testing.T,
	config liveConfig,
	path string,
	header map[string]string,
	body string,
) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, config.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("live lane: raw request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+config.key)
	request.Header.Set("Content-Type", "application/json")
	for name, value := range header {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("live lane: raw request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("live lane: raw response: %v", err)
	}
	return response.StatusCode, raw
}

// TestLiveAsyncImageTask runs the async pipeline end to end: create a task,
// poll it to SUCCEEDED, decode the links, and fetch one.
func TestLiveAsyncImageTask(t *testing.T) {
	config := liveImages(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	size := media.ImageSize{Width: 1024, Height: 1024}
	count := 1
	seed := int64(7)
	watermark := false
	request := userRequest(
		[]message.Part{message.TextPart{Text: "一只橘猫坐在木窗台上，清晨侧光，写实摄影"}},
		inference.Intent{Image: &inference.ImageIntent{
			Size:     &size,
			Count:    &count,
			Seed:     &seed,
			Delivery: media.SourceURL,
		}},
		ImageOptions{Provider: liveProviderID, NegativePrompt: "模糊, 文字", Watermark: &watermark},
	)
	// The media surfaces have no channel for opaque caller metadata. The lane
	// carries some anyway, so the drop is proved on the wire rather than
	// asserted in the abstract: the report says dropped, and the recorded body
	// has to agree.
	request.RequestMetadata = map[string]string{"trace": "live-lane"}
	response, explanation := harness.generate(ctx, "wanx2.1-t2i-turbo", request)

	// The create call: the surface, the mandatory async header, and the body
	// the compile report claimed.
	create := harness.first(imagePathText2Image)
	if create.Method != http.MethodPost {
		t.Errorf("live lane: create method = %s, want POST", create.Method)
	}
	if got := liveHeader(create, headerAsync); got != "enable" {
		t.Errorf("live lane: %s = %q, want enable", headerAsync, got)
	}
	if got := liveHeader(create, "Authorization"); !strings.HasPrefix(got, "Bearer ") {
		t.Errorf("live lane: Authorization = %q, want a bearer token", redact(got))
	}
	body := liveBody(t, create)
	if body["model"] != "wanx2.1-t2i-turbo" {
		t.Errorf("live lane: model = %v", body["model"])
	}
	if prompt := liveNested(t, body, "input", "prompt"); prompt != "一只橘猫坐在木窗台上，清晨侧光，写实摄影" {
		t.Errorf("live lane: input.prompt = %v", prompt)
	}
	if got := liveNested(t, body, "parameters", "size"); got != "1024*1024" {
		t.Errorf("live lane: parameters.size = %v, want 1024*1024", got)
	}
	if got := liveNested(t, body, "parameters", "n"); got != float64(1) {
		t.Errorf("live lane: parameters.n = %v, want 1", got)
	}
	if got := liveNested(t, body, "parameters", "seed"); got != float64(7) {
		t.Errorf("live lane: parameters.seed = %v, want 7", got)
	}
	if got := liveNested(t, body, "input", "negative_prompt"); got != "模糊, 文字" {
		t.Errorf("live lane: input.negative_prompt = %v", got)
	}
	liveDelivered(t, explanation, create, inference.FieldGenerateIntentImageSize, body, "size")
	liveDelivered(t, explanation, create, inference.FieldGenerateIntentImageCount, body, "n")
	liveDelivered(t, explanation, create, liveImageOptionsField("negative_prompt"), body, "negative_prompt")
	liveAbsent(t, explanation, body, inference.FieldGenerateRequestMetadata, "request_metadata")

	// The poll: at least one query, all against the task endpoint.
	queries := 0
	for _, exchange := range harness.exchanges() {
		if strings.HasPrefix(exchange.Path, taskPathPrefix) {
			queries++
		}
	}
	if queries == 0 {
		t.Errorf("live lane: no task query recorded; saw %s", harness.paths())
	}

	// The answer: one image, the platform's own count, and a link that
	// serves what the driver said it decoded.
	links := liveImageParts(t, response)
	if got := liveGenerated(response.Usage.GeneratedImages); got != 1 {
		t.Errorf("live lane: generated images = %d, want 1", got)
	}
	if response.Metadata.RequestID == "" {
		t.Error("live lane: response carries no request id")
	}
	if response.Metadata.ResponseID == "" {
		t.Error("live lane: response carries no task id")
	}
	head := liveArtifact(t, links[0])
	if !isPNG(head) && !isJPEG(head) {
		t.Errorf("live lane: artifact magic = % x, want PNG or JPEG", head[:8])
	}
	t.Logf("live lane: %d image(s), task %s, request %s, artifact %s (% x)",
		len(links), response.Metadata.ResponseID, response.Metadata.RequestID, links[0], head[:4])
}

// TestLiveSyncImage runs the single-call multimodal surface, the shape Wan 2.6
// and later image models serve. Without the interleave option that surface
// edits rather than draws: the platform requires the last message to carry an
// input image.
func TestLiveSyncImage(t *testing.T) {
	config := liveImages(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	base := liveBaseImage(t, harness, ctx)
	size := media.ImageSize{Width: 1280, Height: 1280}
	count := 1
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "把这张照片改成水彩画风格，保留构图"},
			imagePart(t, base),
		},
		inference.Intent{Image: &inference.ImageIntent{
			Size:     &size,
			Count:    &count,
			Delivery: media.SourceURL,
		}},
	)
	response, explanation := harness.generate(ctx, "wan2.6-image", request)

	call := harness.first(imagePathMultimodal)
	if got := liveHeader(call, headerAsync); got != "" {
		t.Errorf("live lane: sync call carries %s = %q", headerAsync, got)
	}
	if got := liveHeader(call, headerSSE); got != "" {
		t.Errorf("live lane: a non-interleaved sync call carries %s = %q, want it left off",
			headerSSE, got)
	}
	body := liveBody(t, call)
	messages := liveNested(t, body, "input", "messages")
	last := liveNested(t, body, "input", "messages", "0", "content", "1", "image")
	if last != base {
		t.Errorf("live lane: input image = %v, want %s", last, base)
	}
	if strings.Contains(fmt.Sprint(messages), "enable_interleave") {
		t.Errorf("live lane: a plain sync call mentions interleave: %v", messages)
	}
	liveDelivered(t, explanation, call, inference.FieldGenerateIntentImageSize, body, "size")
	liveDelivered(t, explanation, call, inference.FieldGenerateInputImage, body, "image")

	links := liveImageParts(t, response)
	head := liveArtifact(t, links[0])
	if !isPNG(head) && !isJPEG(head) {
		t.Errorf("live lane: artifact magic = % x, want PNG or JPEG", head[:8])
	}
	t.Logf("live lane: sync image ok, request %s, artifact %s (% x)",
		response.Metadata.RequestID, links[0], head[:4])
}

// TestLiveInterleaveStream runs the interleaved sync surface: the call has to
// ask for the stream, the platform has to answer with one, and the driver has
// to fold it back into a single result.
func TestLiveInterleaveStream(t *testing.T) {
	config := liveImages(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	size := media.ImageSize{Width: 1280, Height: 1280}
	interleave := true
	request := userRequest(
		[]message.Part{message.TextPart{Text: "画一只在窗台上打盹的白猫，并配一句短诗"}},
		inference.Intent{
			Image: &inference.ImageIntent{Size: &size, Delivery: media.SourceURL},
			Text:  &inference.TextIntent{},
		},
		ImageOptions{Provider: liveProviderID, EnableInterleave: &interleave},
	)
	response, explanation := harness.generate(ctx, "wan2.6-image", request)

	call := harness.first(imagePathMultimodal)
	if got := liveHeader(call, headerSSE); got != "enable" {
		t.Errorf("live lane: %s = %q, want enable", headerSSE, got)
	}
	body := liveBody(t, call)
	liveDelivered(t, explanation, call, liveImageOptionsField("enable_interleave"), body, "enable_interleave")
	if got := liveNested(t, body, "parameters", "enable_interleave"); got != true {
		t.Errorf("live lane: parameters.enable_interleave = %v, want true", got)
	}
	if got := liveNested(t, body, "parameters", "stream"); got != true {
		t.Errorf("live lane: parameters.stream = %v, want the flag paired with the sse header", got)
	}
	if !bytes.Contains(call.Response, []byte("data:")) {
		t.Errorf("live lane: the interleaved surface answered without event framing: %s",
			truncate(call.Response))
	}

	links := liveImageParts(t, response)
	if text := liveText(t, response); text == "" {
		t.Error("live lane: interleaved run decoded no text part")
	}
	head := liveArtifact(t, links[0])
	if !isPNG(head) && !isJPEG(head) {
		t.Errorf("live lane: artifact magic = % x, want PNG or JPEG", head[:8])
	}
	t.Logf("live lane: interleaved run folded %d image(s) and %d text rune(s) out of %d bytes of events",
		len(links), len([]rune(liveText(t, response))), len(call.Response))
}

// TestLiveInterleavedErrorIsClassifiedInBand pins the other half of the
// interleaved surface's protocol: its failures do not arrive as an HTTP error,
// they arrive as an event carrying an error envelope under an HTTP 200. A
// driver that only read the status would report a successful run that produced
// nothing. The lane asks for a model that does not exist, so the refusal is
// guaranteed and the call cannot generate anything.
func TestLiveInterleavedErrorIsClassifiedInBand(t *testing.T) {
	config := liveImages(t)
	client := newMediaClient(
		config.key, config.base, "", Spec{PollIntervalMillis: resource.Int(livePollMillis)})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	declared := ModelSpec{
		Name: "no-such-model-xyz",
		Kind: "image",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText},
			Outputs: []message.PartKind{message.PartImage, message.PartText},
		},
		Image: ImageParams{
			API:         imageAPISync,
			PromptLimit: 2000,
			MinPixels:   1280 * 1280,
			MaxPixels:   1440 * 1440,
		},
	}
	size := media.ImageSize{Width: 1280, Height: 1280}
	interleave := true
	request := userRequest(
		[]message.Part{message.TextPart{Text: "anything at all"}},
		inference.Intent{
			Image: &inference.ImageIntent{Size: &size, Delivery: media.SourceURL},
			Text:  &inference.TextIntent{},
		},
		ImageOptions{Provider: liveProviderID, EnableInterleave: &interleave},
	)
	wire, _, err := compileImageWire(t, declared, request)
	if err != nil {
		t.Fatalf("live lane: compile: %v", err)
	}
	if _, err := transportImage(client, time.Second)(ctx, wire); err == nil {
		t.Fatal("live lane: the platform served a model that does not exist")
	} else if !errdefs.IsValidation(err) {
		t.Errorf("live lane: in-band failure = %v, want an errdefs Validation", err)
	} else {
		t.Logf("live lane: in-band failure classified -> %v", err)
	}
}

// TestLiveImageEdit runs the async editing surface, which reads its input
// through a different door than the text surfaces: a base image, plus the
// function that names the edit.
func TestLiveImageEdit(t *testing.T) {
	config := liveImages(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	base := liveBaseImage(t, harness, ctx)
	size := media.ImageSize{Width: 1024, Height: 1024}
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "把画面改成水彩画风格，保留主体构图"},
			imagePart(t, base),
		},
		inference.Intent{Image: &inference.ImageIntent{Size: &size, Delivery: media.SourceURL}},
		ImageOptions{Provider: liveProviderID, Function: "stylization_all"},
	)
	response, explanation := harness.generate(ctx, "wanx2.1-imageedit", request)

	call := harness.first(imagePathImage2Image)
	if got := liveHeader(call, headerAsync); got != "enable" {
		t.Errorf("live lane: %s = %q, want enable", headerAsync, got)
	}
	body := liveBody(t, call)
	if got := liveNested(t, body, "input", "function"); got != "stylization_all" {
		t.Errorf("live lane: input.function = %v", got)
	}
	if got := liveNested(t, body, "input", "base_image_url"); got != base {
		t.Errorf("live lane: input.base_image_url = %v, want %s", got, base)
	}
	liveDelivered(t, explanation, call, liveImageOptionsField("function"), body, "function")
	liveDelivered(t, explanation, call, inference.FieldGenerateInputImage, body, "base_image_url")

	links := liveImageParts(t, response)
	head := liveArtifact(t, links[0])
	if !isPNG(head) && !isJPEG(head) {
		t.Errorf("live lane: artifact magic = % x, want PNG or JPEG", head[:8])
	}
	t.Logf("live lane: edited %s -> %s (% x)", base, links[0], head[:4])
}

// TestLiveText2Video runs the async video pipeline on the surface whose size
// rides the wire as a concrete resolution.
func TestLiveText2Video(t *testing.T) {
	config := liveVideos(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	duration := int64(5000)
	request := userRequest(
		[]message.Part{message.TextPart{Text: "一只柯基犬在草地上奔跑，镜头跟随，午后阳光"}},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:     "480p",
			AspectRatio:    "16:9",
			DurationMillis: &duration,
		}},
	)
	response, explanation := harness.generate(ctx, "wanx2.1-t2v-turbo", request)

	create := harness.first(videoPathSynthesis)
	if got := liveHeader(create, headerAsync); got != "enable" {
		t.Errorf("live lane: %s = %q, want enable", headerAsync, got)
	}
	body := liveBody(t, create)
	if got := liveNested(t, body, "parameters", "size"); got != "832*480" {
		t.Errorf("live lane: parameters.size = %v, want the concrete 832*480", got)
	}
	if got := liveNested(t, body, "parameters", "duration"); got != float64(5) {
		t.Errorf("live lane: parameters.duration = %v, want 5", got)
	}
	liveDelivered(t, explanation, create, inference.FieldGenerateIntentVideoDuration, body, "duration")

	links := liveVideoParts(t, response)
	if got := liveGenerated(response.Usage.GeneratedVideos); got < 1 {
		t.Errorf("live lane: generated videos = %d, want at least 1", got)
	}
	head := liveArtifact(t, links[0])
	if !isMP4(head) {
		t.Errorf("live lane: artifact magic = % x, want an ISO base media file", head[:12])
	}
	t.Logf("live lane: t2v ok, task %s, request %s, artifact %s (% x)",
		response.Metadata.ResponseID, response.Metadata.RequestID, links[0], head[:12])
}

// TestLiveImageToVideo runs the async video pipeline on the surface that takes
// a source frame, whose size rides the wire as a tier token instead.
func TestLiveImageToVideo(t *testing.T) {
	config := liveVideos(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	base := liveBaseImage(t, harness, ctx)
	duration := int64(5000)
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "镜头缓慢推近，水面泛起涟漪"},
			imagePart(t, base),
		},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:     "480p",
			DurationMillis: &duration,
		}},
	)
	response, explanation := harness.generate(ctx, "wanx2.1-i2v-turbo", request)

	// A source frame rides the same video-generation endpoint as text to
	// video; only the keyframe surface has one of its own.
	create := harness.first(videoPathSynthesis)
	if got := liveHeader(create, headerAsync); got != "enable" {
		t.Errorf("live lane: %s = %q, want enable", headerAsync, got)
	}
	body := liveBody(t, create)
	if got := liveNested(t, body, "input", "img_url"); got != base {
		t.Errorf("live lane: input.img_url = %v, want %s", got, base)
	}
	if got := liveNested(t, body, "parameters", "resolution"); got != "480P" {
		t.Errorf("live lane: parameters.resolution = %v, want the 480P tier", got)
	}
	liveDelivered(t, explanation, create, inference.FieldGenerateInputImage, body, "img_url")

	links := liveVideoParts(t, response)
	head := liveArtifact(t, links[0])
	if !isMP4(head) {
		t.Errorf("live lane: artifact magic = % x, want an ISO base media file", head[:12])
	}
	t.Logf("live lane: i2v ok, task %s, artifact %s (% x)",
		response.Metadata.ResponseID, links[0], head[:12])
}

// TestLiveKeyframesTask runs the keyframe surface, whose two source frames
// ride under field names of their own and whose endpoint is the only one this
// driver serves off the video-generation path.
//
// The lane is opt-in twice over because there is no way to probe the surface
// cheaply: the platform starts rendering within a second of the create call,
// and it only cancels a task that is still PENDING, so the create is the
// purchase. A first attempt at a cancel-based probe answered
// UnsupportedOperation and paid for the clip anyway.
func TestLiveKeyframesTask(t *testing.T) {
	config := liveKeyframes(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	first := liveBaseImage(t, harness, ctx)
	last := liveBaseImage(t, harness, ctx)
	duration := int64(5000)
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "从白天过渡到黄昏，云层流动"},
			imagePart(t, first),
			imagePart(t, last),
		},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:     "720p",
			DurationMillis: &duration,
		}},
	)
	response, explanation := harness.generate(ctx, "wanx2.1-kf2v-plus", request)

	create := harness.first(videoPathImage2)
	if got := liveHeader(create, headerAsync); got != "enable" {
		t.Errorf("live lane: %s = %q, want enable", headerAsync, got)
	}
	body := liveBody(t, create)
	if got := liveNested(t, body, "input", "first_frame_url"); got != first {
		t.Errorf("live lane: input.first_frame_url = %v, want %s", got, first)
	}
	if got := liveNested(t, body, "input", "last_frame_url"); got != last {
		t.Errorf("live lane: input.last_frame_url = %v, want %s", got, last)
	}
	if got := liveNested(t, body, "parameters", "resolution"); got != "720P" {
		t.Errorf("live lane: parameters.resolution = %v, want the 720P tier", got)
	}
	liveDelivered(t, explanation, create, inference.FieldGenerateInputImage, body, "first_frame_url")
	liveDelivered(t, explanation, create, inference.FieldGenerateIntentVideoResolution, body, "resolution")

	links := liveVideoParts(t, response)
	head := liveArtifact(t, links[0])
	if !isMP4(head) {
		t.Errorf("live lane: artifact magic = % x, want an ISO base media file", head[:12])
	}
	t.Logf("live lane: kf2v ok, task %s, artifact %s (% x)",
		response.Metadata.ResponseID, links[0], head[:12])
}

// The Wan 3.0 media-array lanes. The array is the surface no flat key test can
// stand in for — the platform decides whether the entry types, their order, and
// the entry-level rules are right — so these lanes exist to buy that answer
// once. The reference_video and reference_audio entry types stay offline-only:
// a lane can mint an image for itself, and hosting a clip or a track is not
// something these lanes do.

// TestLiveWan3MediaText2Video drives the media surface with a prompt alone:
// the platform's 文生视频 mode, where no media entry rides the body at all.
func TestLiveWan3MediaText2Video(t *testing.T) {
	config := liveWan3(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	duration := int64(2000)
	request := userRequest(
		[]message.Part{message.TextPart{Text: "一只柯基犬在草地上奔跑，镜头跟随，午后阳光"}},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:     "480p",
			DurationMillis: &duration,
		}},
	)
	response, explanation := harness.generate(ctx, "wan3.0-video", request)

	create := harness.first(videoPathSynthesis)
	if got := liveHeader(create, headerAsync); got != "enable" {
		t.Errorf("live lane: %s = %q, want enable", headerAsync, got)
	}
	body := liveBody(t, create)
	if liveHasKey(body["input"], "media") {
		t.Errorf("live lane: a prompt-only request carried a media array: %v", body["input"])
	}
	if got := liveNested(t, body, "parameters", "resolution"); got != "480P" {
		t.Errorf("live lane: parameters.resolution = %v, want the 480P tier", got)
	}
	if got := liveNested(t, body, "parameters", "duration"); got != float64(2) {
		t.Errorf("live lane: parameters.duration = %v, want 2", got)
	}
	liveDelivered(
		t, explanation, create, inference.FieldGenerateIntentVideoDuration, body, "duration")

	links := liveVideoParts(t, response)
	head := liveArtifact(t, links[0])
	if !isMP4(head) {
		t.Errorf("live lane: artifact magic = % x, want an ISO base media file", head[:12])
	}
	t.Logf("live lane: wan3 t2v ok, task %s, artifact %s (% x)",
		response.Metadata.ResponseID, links[0], head[:12])
}

// TestLiveWan3MediaFrames drives the frames mode of the array: the two image
// parts become first_frame and last_frame entries in request order, the call
// stays on the synthesis path (not the 2.x keyframe endpoint), and the
// model-chosen duration rides as -1.
func TestLiveWan3MediaFrames(t *testing.T) {
	config := liveWan3(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	first := liveBaseImage(t, harness, ctx)
	last := liveBaseImage(t, harness, ctx)
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "从白天过渡到黄昏，云层流动"},
			imagePart(t, first),
			imagePart(t, last),
		},
		inference.Intent{Video: &inference.VideoIntent{Resolution: "480p"}},
		VideoOptions{Provider: liveProviderID, SmartDuration: true},
	)
	response, explanation := harness.generate(ctx, "wan3.0-video", request)

	for _, exchange := range harness.exchanges() {
		if exchange.Path == videoPathImage2 {
			t.Errorf("live lane: the frames mode posted to the 2.x keyframe endpoint")
		}
	}
	create := harness.first(videoPathSynthesis)
	if got := liveHeader(create, headerAsync); got != "enable" {
		t.Errorf("live lane: %s = %q, want enable", headerAsync, got)
	}
	body := liveBody(t, create)
	entries, ok := liveNested(t, body, "input", "media").([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("live lane: input.media = %v, want the two frames", body["input"])
	}
	for index, want := range []struct {
		entryType string
		url       string
	}{{"first_frame", first}, {"last_frame", last}} {
		entry, _ := entries[index].(map[string]any)
		if entry["type"] != want.entryType || entry["url"] != want.url {
			t.Errorf("live lane: media[%d] = %v, want %s %s",
				index, entry, want.entryType, want.url)
		}
	}
	if got := liveNested(t, body, "parameters", "duration"); got != float64(-1) {
		t.Errorf("live lane: parameters.duration = %v, want the smart-duration -1", got)
	}
	liveDelivered(t, explanation, create, inference.FieldGenerateInputImage, body, "media")
	liveDelivered(
		t, explanation, create, liveVideoOptionsField("smart_duration"), body, "duration")

	links := liveVideoParts(t, response)
	head := liveArtifact(t, links[0])
	if !isMP4(head) {
		t.Errorf("live lane: artifact magic = % x, want an ISO base media file", head[:12])
	}
	t.Logf("live lane: wan3 frames ok, task %s, artifact %s (% x)",
		response.Metadata.ResponseID, links[0], head[:12])
}

// TestLiveWan3MediaReference drives the reference mode over an array that
// mixes a generated reference image with a public web page, which is also the
// shape that has to leave the platform's prompt rewrite on.
func TestLiveWan3MediaReference(t *testing.T) {
	config := liveWan3(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	still := liveBaseImage(t, harness, ctx)
	duration := int64(2000)
	link := "https://help.aliyun.com/zh/model-studio/wan3-video-generation-api-reference"
	request := userRequest(
		[]message.Part{
			message.TextPart{Text: "把图1里的主体放进一支极简风格的广告片"},
			imagePart(t, still),
		},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:     "480p",
			DurationMillis: &duration,
		}},
		VideoOptions{Provider: liveProviderID, Link: link},
	)
	response, explanation := harness.generate(ctx, "wan3.0-video-refer", request)

	create := harness.first(videoPathSynthesis)
	body := liveBody(t, create)
	if got := body["model"]; got != "wan3.0-video" {
		t.Errorf("live lane: model = %v, want the wire model of the alias", got)
	}
	entries, ok := liveNested(t, body, "input", "media").([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("live lane: input.media = %v, want a reference image and a web page", body["input"])
	}
	first, _ := entries[0].(map[string]any)
	if first["type"] != "reference_image" || first["url"] != still {
		t.Errorf("live lane: media[0] = %v, want the reference image", first)
	}
	second, _ := entries[1].(map[string]any)
	if second["type"] != "link" || second["url"] != link {
		t.Errorf("live lane: media[1] = %v, want the web page", second)
	}
	if liveHasKey(body, "negative_prompt") {
		t.Errorf("live lane: the media wire carried a negative prompt: %v", body["input"])
	}
	liveDelivered(t, explanation, create, liveVideoOptionsField("link"), body, "media")

	links := liveVideoParts(t, response)
	head := liveArtifact(t, links[0])
	if !isMP4(head) {
		t.Errorf("live lane: artifact magic = % x, want an ISO base media file", head[:12])
	}
	t.Logf("live lane: wan3 reference ok, task %s, artifact %s (% x)",
		response.Metadata.ResponseID, links[0], head[:12])
}

// liveBaseImage produces one source image through the async surface and
// returns its link, which is how the lanes that need an input image get one
// without hosting anything: the platform's own output is a public URL.
func liveBaseImage(t *testing.T, harness *liveHarness, ctx context.Context) string {
	t.Helper()
	size := media.ImageSize{Width: 1024, Height: 1024}
	count := 1
	request := userRequest(
		[]message.Part{message.TextPart{Text: "一张构图干净的风光照片，湖面与远山，傍晚"}},
		inference.Intent{Image: &inference.ImageIntent{
			Size:     &size,
			Count:    &count,
			Delivery: media.SourceURL,
		}},
	)
	response, _ := harness.generate(ctx, "wanx2.1-t2i-turbo", request)
	return liveImageParts(t, response)[0]
}

// redact keeps a credential out of a log line while still showing its shape.
func redact(value string) string {
	if value == "" {
		return ""
	}
	return "<redacted>"
}

func truncate(raw []byte) string {
	const limit = 400
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + fmt.Sprintf("... (%d bytes)", len(raw))
}

// ----------------------------------------------------------------- text lane

// TestLiveTextGeneration runs the native text surfaces against the real
// endpoint: a unary generation, a streamed one, and one embedding request the
// declared batch cap splits in two. Both halves of "correct" are checked where
// they can be seen — the endpoint proves the wire shape, the recorded
// transcript proves the driver sent the shape its compile report described.
func TestLiveTextGeneration(t *testing.T) {
	config := liveTexts(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	maxTokens := 32
	thinking := false
	prompt := "用一句话说明什么是流式输出。"
	request := userRequest(
		[]message.Part{message.TextPart{Text: prompt}},
		inference.Intent{Text: &inference.TextIntent{
			MaxOutputTokens:  &maxTokens,
			ReasoningEnabled: &thinking,
		}},
	)

	response, explanation := harness.generate(ctx, "qwen3.8-flash", request)
	if text := strings.TrimSpace(liveText(t, response)); text == "" {
		t.Fatalf("live lane: the unary generation answered no text; answer %s",
			truncate(harness.lastResponse()))
	}
	call := harness.first(generatePathMultimodal)
	if got := liveHeader(call, headerSSE); got != "" {
		t.Errorf("live lane: a unary call carries %s = %q", headerSSE, got)
	}
	body := liveBody(t, call)
	if got := liveNested(t, body, "model"); got != "qwen3.8-flash" {
		t.Errorf("live lane: model = %v", got)
	}
	if got := liveNested(t, body, "input", "messages", "0", "content", "0", "text"); got != prompt {
		t.Errorf("live lane: message content = %v, want %q", got, prompt)
	}
	if liveHasKey(body, "result_format") {
		t.Error("live lane: the multimodal surface carried result_format")
	}
	liveDelivered(t, explanation, call,
		inference.FieldGenerateIntentTextMaxOutputTokens, body, "max_tokens")
	liveDelivered(t, explanation, call,
		inference.FieldGenerateIntentReasoningEnabled, body, "enable_thinking")

	streamed, streamExplanation, deltas := harness.stream(ctx, "qwen3.8-flash", request)
	if deltas == 0 {
		t.Error("live lane: the stream delivered no text delta")
	}
	if text := strings.TrimSpace(liveText(t, streamed)); text == "" {
		t.Errorf("live lane: the stream accumulated no text; answer %s",
			truncate(harness.lastResponse()))
	}
	if streamed.Usage.TotalTokens <= 0 {
		t.Errorf("live lane: the streamed result reported no usage: %+v", streamed.Usage)
	}
	exchanges := harness.exchanges()
	streamCall := exchanges[len(exchanges)-1]
	if got := liveHeader(streamCall, headerSSE); got != "enable" {
		t.Errorf("live lane: a streamed call carries %s = %q, want enable", headerSSE, got)
	}
	streamBody := liveBody(t, streamCall)
	liveDelivered(t, streamExplanation, streamCall,
		inference.FieldGenerateExecutionStream, streamBody, "incremental_output")

	embedRequest := inference.EmbedRequest{
		Items: []inference.EmbedItem{
			embedTextItem("杭州以西湖著称。"),
			embedTextItem("杭州是浙江省的省会。"),
			embedTextItem("杭州位于中国东部。"),
		},
	}
	embedded := harness.embed(ctx, "qwen3.7-text-embedding", embedRequest)
	if len(embedded.Embeddings) != len(embedRequest.Items) {
		t.Fatalf("live lane: %d embeddings for %d items",
			len(embedded.Embeddings), len(embedRequest.Items))
	}
	for index, embedding := range embedded.Embeddings {
		if len(embedding.Vector) == 0 {
			t.Errorf("live lane: embedding %d carries no vector", index)
		}
	}
	if embedded.Usage.InputTokens <= 0 {
		t.Errorf("live lane: the embedding batch reported no tokens: %+v", embedded.Usage)
	}
	batches := 0
	for _, exchange := range harness.exchanges() {
		if exchange.Path == embedPathText {
			batches++
		}
	}
	if batches != 2 {
		t.Errorf("live lane: %d embedding calls, want 2: the declared batch cap is two",
			batches)
	}
	t.Logf("live lane: text ok — chat %q, stream %d deltas / %d tokens, embedding dim %d",
		truncate([]byte(liveText(t, response))), deltas, streamed.Usage.TotalTokens,
		len(embedded.Embeddings[0].Vector))
}

// --------------------------------------------------------------- speech lane

// TestLiveSpeechSynthesis runs the native speech surface against the real
// endpoint: the request shape the platform documents, the answer's URL, and —
// the reason the lane downloads it — the payload the declaration claims to
// serve, which only the platform's own bytes can confirm.
func TestLiveSpeechSynthesis(t *testing.T) {
	config := liveSpeech(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	request := userRequest(
		[]message.Part{message.TextPart{Text: "今天天气真不错，适合出去走走。"}},
		inference.Intent{Audio: &inference.AudioIntent{
			Voice: media.VoiceSpec{ID: "Cherry", Language: "Chinese"},
			Format: media.AudioFormat{
				Encoding:     media.AudioEncodingPCM16,
				SampleRateHz: 24000,
				Channels:     1,
			},
		}},
	)
	response, explanation := harness.generate(ctx, "qwen3-tts-flash", request)
	call := harness.first(generatePathMultimodal)
	if got := liveHeader(call, headerSSE); got != "" {
		t.Errorf("live lane: a unary speech call carries %s = %q", headerSSE, got)
	}
	body := liveBody(t, call)
	if got := liveNested(t, body, "model"); got != "qwen3-tts-flash" {
		t.Errorf("live lane: model = %v", got)
	}
	if got := liveNested(t, body, "input", "voice"); got != "Cherry" {
		t.Errorf("live lane: input.voice = %v", got)
	}
	if got := liveNested(t, body, "input", "language_type"); got != "Chinese" {
		t.Errorf("live lane: input.language_type = %v", got)
	}
	if liveHasKey(body, "parameters") {
		t.Errorf("live lane: the speech surface carried a parameters block: %s", call.Body)
	}
	liveDelivered(t, explanation, call,
		inference.FieldGenerateIntentAudioVoice, body, "voice")
	// The format asks ride no wire field: the surface has no control for
	// them, and the driver answers with the shape the declaration states.
	for _, field := range []inference.FieldID{
		inference.FieldGenerateIntentAudioFormatEncoding,
		inference.FieldGenerateIntentAudioFormatSampleRate,
		inference.FieldGenerateIntentAudioFormatChannels,
	} {
		liveNative(t, explanation, field)
	}

	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("live lane: parts = %#v", response.Message.Content.Parts)
	}
	audio, ok := response.Message.Content.Parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("live lane: part = %T", response.Message.Content.Parts[0])
	}
	if audio.Source.Kind() != media.SourceURL {
		t.Fatalf("live lane: source kind = %v", audio.Source.Kind())
	}
	if audio.Format == nil || *audio.Format != (media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 24000,
		Channels:     1,
	}) {
		t.Errorf("live lane: format = %+v", audio.Format)
	}
	payload := liveArtifact(t, audio.Source.URL())
	channels, rate, bits := liveWAVFormat(t, payload)
	if channels != 1 || rate != 24000 || bits != 16 {
		t.Errorf(
			"live lane: the platform served %d ch / %d Hz / %d bit; the declaration says 1 / 24000 / 16",
			channels, rate, bits)
	}
	t.Logf("live lane: speech ok — %d bytes of %d Hz mono %d-bit PCM in a WAV container",
		len(payload), rate, bits)
}

// liveNative asserts the report called a field native without naming a wire
// key: the surface carries no control for it, and the answer's shape — which
// the lane reads back — is what stands in for the field.
func liveNative(
	t *testing.T,
	explanation inference.Explanation,
	field inference.FieldID,
) {
	t.Helper()
	decision, ok := decisionFor(explanationReport(t, explanation), field)
	if !ok {
		t.Fatalf("live lane: %s carries no decision", field)
	}
	if decision.Disposition != inference.Native {
		t.Errorf("live lane: %s disposition = %s (%s), want native",
			field, decision.Disposition, decision.Reason)
	}
}

// liveWAVFormat reads a RIFF/WAVE payload's format chunk: the served rate,
// channel count, and sample width are facts only the bytes can settle.
func liveWAVFormat(t *testing.T, payload []byte) (channels, rate, bits int) {
	t.Helper()
	if len(payload) < 44 || string(payload[:4]) != "RIFF" ||
		string(payload[8:12]) != "WAVE" {
		head := payload
		if len(head) > 16 {
			head = head[:16]
		}
		t.Fatalf("live lane: the answer is not a RIFF/WAVE file: % x", head)
	}
	index := bytes.Index(payload, []byte("fmt "))
	if index < 0 || index+24 > len(payload) {
		t.Fatalf("live lane: the RIFF file carries no readable fmt chunk")
	}
	value := payload[index+8:]
	channels = int(binary.LittleEndian.Uint16(value[2:4]))
	rate = int(binary.LittleEndian.Uint32(value[4:8]))
	bits = int(binary.LittleEndian.Uint16(value[14:16]))
	return channels, rate, bits
}

// ------------------------------------------------------- transcription lane

// TestLiveFileTranscription runs the native file-transcription pipeline
// against the real endpoint: the submitted file URL, the task poll, the
// result file the driver fetches from the platform's own host, and the
// transcript the declaration promises to serve. The sample is the platform's
// own public one, because the surface requires a URL the endpoint can fetch
// itself.
func TestLiveFileTranscription(t *testing.T) {
	config := liveTranscripts(t)
	harness := newLiveHarness(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const sample = "https://dashscope.oss-cn-beijing.aliyuncs.com/samples/audio/paraformer/hello_world_female2.wav"
	request := inference.TranscriptionRequest{
		Audio:      audioURLSource(t, sample),
		Language:   "zh",
		Prompt:     "阿里巴巴 达摩院",
		Timestamps: true,
	}
	response, explanation := harness.transcribe(ctx, "paraformer-v2", request)
	if text := strings.TrimSpace(response.Text); text == "" {
		t.Fatalf("live lane: the transcription answered no text; answer %s",
			truncate(harness.lastResponse()))
	}
	if len(response.Segments) == 0 {
		t.Error("live lane: the transcript carries no segments")
	}
	if response.Language != "zh" {
		t.Errorf("live lane: language = %q, want the hint the request carried",
			response.Language)
	}
	duration := int64(0)
	if response.DurationMillis == nil || *response.DurationMillis <= 0 {
		t.Errorf("live lane: duration = %v, want the source duration",
			response.DurationMillis)
	} else {
		duration = *response.DurationMillis
	}
	if response.Usage.AudioDurationMillis == nil ||
		*response.Usage.AudioDurationMillis <= 0 {
		t.Errorf("live lane: billed duration = %v, want the task's usage",
			response.Usage.AudioDurationMillis)
	}
	call := harness.first(transcribePath)
	if got := liveHeader(call, headerAsync); got != "enable" {
		t.Errorf("live lane: the create call carries %s = %q, want enable",
			headerAsync, got)
	}
	body := liveBody(t, call)
	if got := liveNested(t, body, "model"); got != "paraformer-v2" {
		t.Errorf("live lane: model = %v", got)
	}
	if got := liveNested(t, body, "input", "file_urls", "0"); got != sample {
		t.Errorf("live lane: file url = %v, want %q", got, sample)
	}
	if got := liveNested(t, body, "parameters", "language_hints", "0"); got != "zh" {
		t.Errorf("live lane: language hint = %v", got)
	}
	liveDelivered(t, explanation, call,
		inference.FieldTranscriptionAudio, body, "file_urls")
	liveDelivered(t, explanation, call,
		inference.FieldTranscriptionLanguage, body, "language_hints")
	liveDelivered(t, explanation, call,
		inference.FieldTranscriptionPrompt, body, "context")
	liveDelivered(t, explanation, call,
		inference.FieldTranscriptionTimestamps, body, "timestamp_alignment_enabled")
	t.Logf("live lane: transcription ok — %d segments, %d characters, duration %d ms",
		len(response.Segments), len([]rune(response.Text)), duration)
}
