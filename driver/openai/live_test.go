package openai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
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
	"github.com/GizClaw/flowcraft/core/resource"
)

// The live lanes drive the real driver against the real OpenAI endpoint.
// Everything else in this package asserts what the platform documents; these
// lanes assert what it does.
//
// The Decisions surface is in public beta behind model access, so one lane
// probes before the other buys: a model name the surface does not publish
// draws a refusal that costs nothing, and the refusal is what tells the lane
// the route is reachable for this account and how it fails. The answered
// request that follows spends a few hundred tokens on one small call.
//
// The probe's failure is free, but the paid call is a purchase, so the lanes
// are gated twice: a key has to resolve, and the paying lane has to be opted
// into on top of that. An .env with a key in it must not turn `make test` into
// a bill.
//
//	OPENAI_API_KEY      environment or repo-root .env; unset => every lane skips
//	OPENAI_LIVE=1       the lane that buys one answered decision request
//	OPENAI_BASE_URL     host override; defaults to the public API
//	OPENAI_LIVE_DUMP    directory to keep the recorded transcripts in
//
// The lanes run the provider the way a deployment assembles it (the public
// resource factory) but root it at a recording proxy in front of the real
// host. The endpoint that answers is still the real one, so an accepted call
// proves the wire shape is right, and the recording proves the driver sent the
// shape its compile report said it would — the two halves of "correct" that a
// smoke test alone cannot separate.
const (
	envLiveKey  = "OPENAI_API_KEY"
	envLiveCall = "OPENAI_LIVE"
	envLiveBase = "OPENAI_BASE_URL"
	envLiveDump = "OPENAI_LIVE_DUMP"

	// liveProviderID is the settings id the lanes' provider builds under; it
	// is unrelated to providerID, which names the module's own default.
	liveProviderID = "openai-live"
	// liveProfileID is the single credential profile the lanes declare.
	liveProfileID = "live"
	// liveDecisionModel names the one model the Decisions surface publishes
	// today. It is a declaration like any other: the driver ships no
	// line-up, so the lane states the model it addresses.
	liveDecisionModel = "gpt-6-luna"
	// liveProbeModel is deliberately not a model. The probe's question is
	// what the surface does with a name it does not serve, and nothing can
	// be generated under it, so asking is free.
	liveProbeModel = "gpt-6-luna-unpublished"
	// liveDecisionPath is the route the workload lowers onto, relative to
	// the configured base URL — what the recording should hold.
	liveDecisionPath = "/decisions"

	// liveDecisionEvidence is the evidence the paying lane evaluates: one
	// short customer complaint, enough for a predicate, two choices, and a
	// score to have something to talk about.
	liveDecisionEvidence = "Order placed at 19:05. It arrived 45 minutes " +
		"late, the soup had leaked through the bag, and the driver never " +
		"rang the bell."
)

// ------------------------------------------------------------------- gating

// liveConfig is one lane's resolved credential and host.
type liveConfig struct {
	key  string
	base string
}

// liveCredential resolves the settings every lane needs: the profile secret
// and the host. A missing key skips the lane rather than failing it, because
// the offline suite is the gate a machine without credentials still runs.
func liveCredential(t *testing.T) liveConfig {
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

// liveDecision gates the lane that buys one answered request. The call is
// small — a decision request over one paragraph — but it is still a call, so
// it asks to be opted into like the other drivers' generation lanes.
func liveDecision(t *testing.T) liveConfig {
	t.Helper()
	config := liveCredential(t)
	if liveValue(envLiveCall) == "" {
		t.Skipf("live lane: %s is unset; skipping the paid call", envLiveCall)
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

// nonEmpty resolves a setting with a default.
func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
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
// streams responses through as they arrive while copying every byte it passes
// into the recorder, and it returns the URL the deployment should be rooted at.
//
// The upstream may carry its own base path — the public API's /v1 — so the
// request path is appended after it rather than replacing it: the driver
// spells routes relative to the base URL it was configured with, and the
// recording keeps exactly that spelling.
func newLiveProxy(t *testing.T, upstreamBase string) (string, *liveRecorder) {
	t.Helper()
	upstream, err := url.Parse(upstreamBase)
	if err != nil {
		t.Fatalf("live proxy: upstream %q: %v", upstreamBase, err)
	}
	recorder := &liveRecorder{}
	// The proxy speaks HTTP/1.1 to the upstream. The lane has no stake in
	// which protocol carries the request, and pinning it to the older one
	// removes a variable: this endpoint drops some HTTP/2 connections
	// outright, which reaches the recording as a bare EOF rather than an
	// answer.
	client := &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
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
		target := upstream.Scheme + "://" + upstream.Host +
			joinPath(upstream.Path, r.URL.Path)
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
		// Ask for an identity response so the recording holds the endpoint's
		// own JSON rather than a compressed stream of it.
		outbound.Header.Del("Accept-Encoding")
		response, err := client.Do(outbound)
		if err != nil {
			exchange.Status = http.StatusBadGateway
			// Keep the failure in the recording: a transcript that says
			// "502" and nothing else cannot be debugged.
			exchange.Response = []byte(fmt.Sprintf(
				"proxy: %v (outbound %s %s)", err, outbound.Method, outbound.URL))
			recorder.add(exchange)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = response.Body.Close() }()
		exchange.Status = response.StatusCode
		copyHeader(w.Header(), response.Header)
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

// joinPath appends a request path to the upstream's base path, keeping exactly
// one slash between them.
func joinPath(base, path string) string {
	switch {
	case base == "" || base == "/":
		return path
	case strings.HasSuffix(base, "/") && strings.HasPrefix(path, "/"):
		return base + path[1:]
	case strings.HasSuffix(base, "/") || strings.HasPrefix(path, "/"):
		return base + path
	}
	return base + "/" + path
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

func newLiveHarness(t *testing.T, config liveConfig, models ...ModelSpec) *liveHarness {
	t.Helper()
	base, recorder := newLiveProxy(t, config.base)
	rawSpec, err := json.Marshal(Spec{
		Endpoint: EndpointSpec{BaseURL: base},
		Models:   models,
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
			"id":         liveProfileID,
			"operations": []string{string(model.OperationDecision)},
			"secrets":    map[string]any{SecretAPIKey: config.key},
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

// liveReference addresses one declared model under the lane's provider and
// profile.
func liveReference(name string) model.ModelRef {
	return model.ModelRef{
		ID:      model.ModelID{Provider: liveProviderID, Name: name},
		Profile: liveProfileID,
	}
}

// decision runs one decision call through the assembled driver: compile, then
// execute against the real endpoint. The error is returned rather than fataled,
// because the probe lane's whole subject is the refusal.
func (h *liveHarness) decision(
	ctx context.Context,
	name string,
	request inference.DecisionRequest,
) (inference.DecisionResponse, inference.Explanation, error) {
	h.t.Helper()
	reference := liveReference(name)
	openers := h.openers(name)
	if openers.Decision == nil {
		h.t.Fatalf("live harness: %s declares no decision driver", name)
	}
	driver, err := openers.Decision(ctx, reference)
	if err != nil {
		h.t.Fatalf("live harness: open %s: %v", name, err)
	}
	prepared, err := driver.Prepare(ctx, reference, request)
	if err != nil {
		h.t.Fatalf("live harness: prepare %s: %v", name, err)
	}
	explanation := prepared.Explanation()
	response, err := prepared.Execute(ctx)
	return response, explanation, err
}

// lastResponse returns the endpoint's last recorded answer, which is what a
// failure message needs to be readable.
func (h *liveHarness) lastResponse() []byte {
	exchanges := h.exchanges()
	if len(exchanges) == 0 {
		return nil
	}
	return exchanges[len(exchanges)-1].Response
}

// exchanges returns the calls this lane made.
func (h *liveHarness) exchanges() []liveExchange { return h.recorder.all() }

// last returns the last exchange on one route. The SDK retries inside a single
// driver call, and a retried attempt is a transaction the endpoint refused, so
// the exchange that answers the lane's questions is the last one: reading the
// first would assert against an attempt that was thrown away.
func (h *liveHarness) last(path string) liveExchange {
	h.t.Helper()
	exchanges := h.exchanges()
	for index := len(exchanges) - 1; index >= 0; index-- {
		if exchanges[index].Path == path {
			return exchanges[index]
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

// --------------------------------------------------------------- probe lane

// TestLiveDecisionUnknownModelIsRejected is the free probe: one well-formed
// request naming a model the surface does not publish. What the surface does
// with it is the point — a route that exists reads the model and refuses it,
// while a route that does not exist answers about the URL, and the two are
// distinguishable without spending anything.
func TestLiveDecisionUnknownModelIsRejected(t *testing.T) {
	config := liveCredential(t)
	harness := newLiveHarness(t, config, liveDecisionDeclaration(liveProbeModel))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	probe := inference.DecisionRequest{
		Input: inference.DecisionInput{Text: "probe"},
		Questions: []inference.DecisionQuestion{{
			Name:         "probe",
			Kind:         inference.DecisionQuestionPredicate,
			Instructions: "Is this probe answered?",
		}},
	}
	_, _, err := harness.decision(ctx, liveProbeModel, probe)
	if err == nil {
		t.Fatalf("live lane: the surface answered a model it does not publish: %s",
			truncate(harness.lastResponse()))
	}
	if errdefs.IsUnauthorized(err) {
		t.Fatalf("live lane: the key was refused: %v", err)
	}
	switch {
	case errdefs.IsValidation(err):
		// The surface read the request and refused the model: the route is
		// reachable, and an unusable model is a permanent failure rather
		// than a retryable one.
	case errdefs.IsForbidden(err):
		// A refusal about the account (beta access) rather than the request;
		// the paying lane reports the same shape with a model it can name.
	default:
		t.Fatalf("live lane: unexpected failure shape: %v", err)
	}
	call := harness.last(liveDecisionPath)
	if call.Method != http.MethodPost {
		t.Errorf("live lane: %s with %s, want POST", liveDecisionPath, call.Method)
	}
	if strings.Contains(string(call.Response), "Invalid URL") {
		t.Fatalf("live lane: the surface does not serve %s: %s",
			liveDecisionPath, truncate(call.Response))
	}
	t.Logf("live lane: probe %s -> HTTP %d: %s",
		liveProbeModel, call.Status, truncate(call.Response))
}

// ---------------------------------------------------------------- paid lane

// TestLiveDecision runs one answered decision request against the real
// endpoint: evidence in, one question of each kind out, and the recorded
// transcript checked against the compile report for the fields the report
// claims reached the wire.
//
// The request deliberately carries a choice question whose options are typed:
// a boolean option has to ride the wire as a boolean and come back a boolean,
// which is the one thing about this surface a string-only test cannot see.
func TestLiveDecision(t *testing.T) {
	config := liveDecision(t)
	harness := newLiveHarness(t, config, liveDecisionDeclaration(liveDecisionModel))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	request := liveDecisionRequest()
	response, explanation, err := harness.decision(ctx, liveDecisionModel, request)
	if err != nil {
		t.Fatalf("live lane: decision: %v; last answer: %s",
			err, truncate(harness.lastResponse()))
	}

	if len(response.Answers) != len(request.Questions) {
		t.Fatalf("live lane: %d answers for %d questions",
			len(response.Answers), len(request.Questions))
	}
	if response.Model == "" {
		t.Error("live lane: the answer carries no model id")
	}
	if response.Usage.InputTokens <= 0 || response.Usage.TotalTokens <= 0 {
		t.Errorf("live lane: the answer reported no usage: %+v", response.Usage)
	}

	refusals := 0
	for index, question := range request.Questions {
		answer := response.Answers[index]
		if answer.Name != question.Name {
			t.Errorf("live lane: answer %d is named %q, want %q",
				index, answer.Name, question.Name)
			continue
		}
		if answer.Kind == inference.DecisionAnswerRefusal {
			refusals++
			continue
		}
		if want := inference.DecisionAnswerKind(question.Kind); answer.Kind != want {
			t.Errorf("live lane: answer %q is a %s, want a %s",
				answer.Name, answer.Kind, want)
		}
	}
	boolChoice := liveAnswer(t, response, "escalate")
	if boolChoice.Kind == inference.DecisionAnswerChoice {
		if boolChoice.Choice == nil ||
			boolChoice.Choice.Kind != inference.DecisionChoiceValueBoolean {
			t.Errorf("live lane: the boolean choice answered %v, want a boolean",
				boolChoice.Choice)
		}
	}

	call := harness.last(liveDecisionPath)
	if call.Method != http.MethodPost {
		t.Errorf("live lane: %s with %s, want POST", liveDecisionPath, call.Method)
	}
	body := liveBody(t, call)
	if got := body["model"]; got != liveDecisionModel {
		t.Errorf("live lane: model = %v, want %q", got, liveDecisionModel)
	}
	if got := body["input"]; got != liveDecisionEvidence {
		t.Errorf("live lane: input = %v, want the evidence string", got)
	}
	if got := body["safety_identifier"]; got != "live-lane" {
		t.Errorf("live lane: safety_identifier = %v, want live-lane", got)
	}
	questions, ok := body["questions"].([]any)
	if !ok || len(questions) != len(request.Questions) {
		t.Fatalf("live lane: questions = %v", body["questions"])
	}
	for index, question := range request.Questions {
		got := liveNested(t, body, "questions", strconv.Itoa(index), "name")
		if got != question.Name {
			t.Errorf("live lane: question %d is named %v, want %q",
				index, got, question.Name)
		}
	}
	if got := liveNested(t, body, "questions", "2", "choices", "0", "value"); got != true {
		t.Errorf("live lane: the boolean option rode the wire as %#v, want true", got)
	}
	if got := liveNested(t, body, "questions", "3", "levels", "0", "label"); got != "minor" {
		t.Errorf("live lane: level 0 is labelled %v, want minor", got)
	}
	liveDelivered(t, explanation, call,
		inference.FieldDecisionInputText, body, "input")
	liveDelivered(t, explanation, call,
		inference.FieldDecisionQuestionName, body, "name")
	liveDelivered(t, explanation, call,
		inference.FieldDecisionQuestionKind, body, "type")
	liveDelivered(t, explanation, call,
		inference.FieldDecisionQuestionChoices, body, "choices")
	liveDelivered(t, explanation, call,
		inference.FieldDecisionQuestionLevels, body, "levels")
	liveDelivered(t, explanation, call,
		inference.ExtensionField("safety_identifier").Qualify(
			DecisionOptions{Provider: liveProviderID}),
		body, "safety_identifier")

	t.Logf("live lane: decision ok — %d answers (%d refusals), model %q, %d tokens",
		len(response.Answers), refusals, response.Model, response.Usage.TotalTokens)
}

// liveDecisionDeclaration declares one decision model under the name the lane
// addresses it by: the driver ships no line-up, so a lane that wants a model
// has to state it, exactly as a deployment would.
func liveDecisionDeclaration(name string) ModelSpec {
	return testModel(name, kindDecision, decisionCapabilities()).spec
}

// liveDecisionRequest is the one request the paying lane buys: evidence plus
// one question of each kind, a typed choice whose options are booleans, and
// the safety identifier — the one surface setting with no canonical field.
func liveDecisionRequest() inference.DecisionRequest {
	return inference.DecisionRequest{
		Input: inference.DecisionInput{Text: liveDecisionEvidence},
		Questions: []inference.DecisionQuestion{
			{
				Name:         "complaint",
				Kind:         inference.DecisionQuestionPredicate,
				Instructions: "Is this customer review a complaint?",
			},
			{
				Name:         "sentiment",
				Kind:         inference.DecisionQuestionChoice,
				Instructions: "What is the overall sentiment of the order?",
				Choices: []inference.DecisionChoice{
					{Value: inference.NewDecisionStringChoice("negative")},
					{Value: inference.NewDecisionStringChoice("positive")},
				},
			},
			{
				Name:         "escalate",
				Kind:         inference.DecisionQuestionChoice,
				Instructions: "Should a support agent call the customer back?",
				Choices: []inference.DecisionChoice{
					{Value: inference.NewDecisionBooleanChoice(true)},
					{Value: inference.NewDecisionBooleanChoice(false)},
				},
			},
			{
				Name:         "severity",
				Kind:         inference.DecisionQuestionScore,
				Instructions: "How severe is the problem this order had?",
				Levels: []inference.DecisionLevel{
					{Label: "minor", Description: "A small inconvenience"},
					{Label: "moderate"},
					{Label: "severe"},
				},
			},
		},
		Extensions: inference.Extensions{
			// The option names the deployment it belongs to: an option that
			// does not name its provider is inert rather than invalid, and a
			// lane using the module default would quietly assert nothing.
			DecisionOptions{
				Provider:         liveProviderID,
				SafetyIdentifier: "live-lane",
			},
		},
	}
}

// ---------------------------------------------------------------- assertions

// liveAnswer returns the answer one question's name locates, which is how
// every consumer reads a decision response — never by position.
func liveAnswer(
	t *testing.T,
	response inference.DecisionResponse,
	name string,
) inference.DecisionAnswer {
	t.Helper()
	for _, answer := range response.Answers {
		if answer.Name == name {
			return answer
		}
	}
	t.Fatalf("live lane: no answer named %q; got %s", name, liveAnswerNames(response))
	return inference.DecisionAnswer{}
}

func liveAnswerNames(response inference.DecisionResponse) string {
	names := make([]string, 0, len(response.Answers))
	for _, answer := range response.Answers {
		names = append(names, answer.Name+" ("+string(answer.Kind)+")")
	}
	return strings.Join(names, ", ")
}

func explanationReport(t *testing.T, explanation inference.Explanation) inference.CompileReport {
	t.Helper()
	return inference.CompileReport{Decisions: explanation.Decisions}
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
			t.Fatalf("live lane: recorded body carries no %s",
				strings.Join(path[:index+1], "."))
		}
	}
	return current
}

// liveHasKey reports whether a decoded body carries a key at any depth, which
// is how a lane checks a field rode the wire.
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
	decision, ok := decisionFor(explanationReport(t, explanation).Decisions, field)
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

func truncate(raw []byte) string {
	const limit = 400
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + fmt.Sprintf("... (%d bytes)", len(raw))
}
