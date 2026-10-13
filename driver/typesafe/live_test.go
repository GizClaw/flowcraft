package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// The live lanes drive the driver against TypeSafe's real route. Everything
// else in this package asserts what the vendor published; these lanes assert
// what the vendor answers. They are the half a fixture cannot be: the vendor
// facts this driver encodes are read back off the vendor's own document and off
// the route itself, so a fact that drifts fails here instead of at a
// deployment.
//
// They are also the only lanes that need the network — and one of them needs a
// credential — so none of them is gated on the credential alone. `make test`
// has to run on a machine with no key and no route:
//
//	TYPESAFE_LIVE=1     opt in to the live lanes: the schema check and the
//	                    free credential probe (no key needed)
//	TYPESAFE_API_KEY    environment or repo-root .env; only the lane that buys
//	                    one answered decision request needs it
//	TYPESAFE_BASE_URL   host override; defaults to the public API
//	TYPESAFE_MODEL      model name to address; defaults to the vendor's alias
//
// The probe is free because the route refuses a missing or wrong key before it
// evaluates anything, so the lane runs the real transport, the real failure
// path, and the real status mapping without spending a token. The schema lane
// reads the vendor's published OpenAPI document, which is what the driver's
// comments cite: the state shapes, the three question and answer kinds, the
// required fields, and the usage shape.
const (
	envLive      = "TYPESAFE_LIVE"
	envLiveKey   = "TYPESAFE_API_KEY"
	envLiveBase  = "TYPESAFE_BASE_URL"
	envLiveModel = "TYPESAFE_MODEL"

	// liveProviderID is the settings id the lanes' provider builds under; it
	// is unrelated to providerID, which names the module's own default.
	liveProviderID = "typesafe-live"
	// liveProfileID is the single credential profile the lanes declare.
	liveProfileID = "live"
	// liveUnknownKey is what the probe authenticates with: a key that cannot
	// be valid, which is the failure it is asking for.
	liveUnknownKey = "sk-live-probe-not-a-key"
	// liveSchemaPath is where the vendor publishes the OpenAPI document, one
	// level above the version root the route lives under.
	liveSchemaPath = "/openapi.json"
)

// liveConfig is one lane's resolved base URL and model.
type liveConfig struct {
	base  string
	model string
}

// liveEnabled gates every lane on the explicit opt-in. A key alone is not
// enough: the schema lane and the probe need no credential, so gating them on
// one would leave them unrunnable on the machines that most need them — and
// gating them on nothing would put the vendor's availability into `make test`.
func liveEnabled(t *testing.T) liveConfig {
	t.Helper()
	if strings.TrimSpace(liveValue(envLive)) == "" {
		t.Skipf("live lane: %s is unset; skipping the lanes that need the network",
			envLive)
	}
	return liveConfig{
		base:  nonEmpty(liveValue(envLiveBase), DefaultBaseURL),
		model: nonEmpty(liveValue(envLiveModel), DefaultModel),
	}
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

// readEnvFile reads NAME=value lines out of one .env file, ignoring comments
// and blank lines. A file that does not exist is not an error: the lanes skip
// without one.
func readEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values, nil
}

// nonEmpty returns value, or fallback when value is empty.
func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// liveProviderSettings renders one deployment document over the lane's base URL
// and model, with the given key.
func liveProviderSettings(t *testing.T, config liveConfig, key string) string {
	t.Helper()
	declared, err := json.Marshal([]ModelSpec{decisionSpec(config.model)})
	if err != nil {
		t.Fatalf("marshal declarations: %v", err)
	}
	endpoint, err := json.Marshal(EndpointSpec{BaseURL: config.base})
	if err != nil {
		t.Fatalf("marshal endpoint: %v", err)
	}
	secret, err := json.Marshal(map[string]string{SecretAPIKey: key})
	if err != nil {
		t.Fatalf("marshal secret: %v", err)
	}
	return `{
		"id": "` + liveProviderID + `",
		"spec": {"models": ` + string(declared) + `, "endpoint": ` + string(endpoint) + `},
		"profiles": [{"id": "` + liveProfileID + `", "operations": ["decision"],
			"secrets": ` + string(secret) + `}]
	}`
}

// liveDecisionDriver builds the driver the way a deployment does — the public
// resource factory, then the opener of the model the provider declares — so a
// lane exercises the assembly path and not a private shortcut.
func liveDecisionDriver(
	t *testing.T,
	config liveConfig,
	key string,
) inference.DecisionDriver {
	t.Helper()
	ctx := context.Background()
	provider, err := newProvider(t, liveProviderSettings(t, config, key))
	if err != nil {
		t.Fatalf("build provider: %v", err)
	}
	if len(provider.Models) != 1 {
		t.Fatalf("provider declares %d models, want the one the lane asked for",
			len(provider.Models))
	}
	implementation := provider.Models[0]
	ref := model.ModelRef{
		ID:      implementation.Descriptor.ID,
		Profile: liveProfileID,
	}
	driver, err := implementation.Openers.Decision(ctx, ref)
	if err != nil {
		t.Fatalf("open decision driver: %v", err)
	}
	return driver
}

// TestLiveCredentialProbe runs the real transport against the real route with a
// key that cannot be valid. The route refuses before it evaluates anything, so
// the failure costs nothing and pins four things a fixture cannot: that the
// route is where the driver posts, that the vendor's failures carry
// x-typesafe-request-id, that the error body is the documented envelope, and
// that the driver classifies a wrong key as unauthorized rather than as a
// transient failure the router would retry.
func TestLiveCredentialProbe(t *testing.T) {
	config := liveEnabled(t)
	ctx := context.Background()
	driver := liveDecisionDriver(t, config, liveUnknownKey)

	_, err := driver.Execute(ctx, liveModelRef(t), decisionRequestFixture())
	if err == nil {
		t.Fatal("the route accepted a key that cannot be valid")
	}
	if !errdefs.IsUnauthorized(err) {
		t.Errorf("error = %v, want an unauthorized failure", err)
	}
	if errdefs.IsNotAvailable(err) {
		t.Errorf("error = %v, want a failure the router does not retry", err)
	}
	cause := liveCause(t, err)
	if !strings.Contains(cause, "authentication_error") {
		t.Errorf("cause = %v, want the vendor's own error type in it", cause)
	}
	id, ok := errdefs.RequestID(err)
	if !ok || !strings.HasPrefix(id, "req_") {
		t.Errorf("request id = %q (%v), want the x-typesafe-request-id header",
			id, ok)
	}
	t.Logf("probe: %v (request id %s)", cause, id)

	// The route answers a *missing* key with 403 where its status table names
	// 401 for "missing or invalid API key". A deployment cannot reach that
	// path through this driver — a profile with no key fails when it opens —
	// so the fact is pinned here, raw, where it can be seen.
	status, header, body := liveRawFailure(t, config, "")
	if status != http.StatusForbidden {
		t.Errorf("no key: HTTP %d, want 403", status)
	}
	if !strings.Contains(body, "authentication_error") {
		t.Errorf("no key: body = %s, want the vendor's error type", body)
	}
	if !strings.HasPrefix(header, "req_") {
		t.Errorf("no key: request id = %q, want the vendor's header", header)
	}
	t.Logf("probe: no key -> HTTP %d %s", status, body)
}

// liveRawFailure posts an empty body to the route and returns the status, the
// vendor's request id header, and the body, so a lane can pin what the wire
// answers without a driver in the way.
func liveRawFailure(t *testing.T, config liveConfig, key string) (int, string, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost,
		strings.TrimRight(config.base, "/")+systemOnePath,
		strings.NewReader(`{}`),
	)
	if err != nil {
		t.Fatalf("build probe request: %v", err)
	}
	request.Header.Set(headerContentType, "application/json")
	if key != "" {
		request.Header.Set(headerAuthorization, "Bearer "+key)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("probe %s: %v", request.URL, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		t.Fatalf("read probe body: %v", err)
	}
	return response.StatusCode, response.Header.Get(requestIDHeader),
		strings.TrimSpace(string(body))
}

// TestLiveSchemaCarriesTheFactsTheDriverEncodes reads the vendor's published
// OpenAPI document and checks the facts this driver's comments and compiler
// rest on. It is the answer to "asserted from this package's own fixture": the
// document is the vendor's, and a change to it fails here rather than at a
// deployment that routes a decision workload.
//
// The two numeric bounds the driver inherits — 255 options per Choice and 10
// levels per Score — are published in prose rather than in this schema, so they
// are cited in decision.go instead of asserted here; so is the 64k-token
// context budget, which is a model capacity rather than a route limit.
func TestLiveSchemaCarriesTheFactsTheDriverEncodes(t *testing.T) {
	config := liveEnabled(t)
	schema := liveSchema(t, config.base)

	// The document publishes the route the driver posts to: the base URL's own
	// path plus the route name, which is the arithmetic the transport does.
	parsed, err := url.Parse(config.base)
	if err != nil {
		t.Fatalf("base url %q: %v", config.base, err)
	}
	route := strings.TrimSuffix(parsed.Path, "/") + systemOnePath
	if _, ok := liveLookup(t, schema, "paths", route)["post"]; !ok {
		t.Errorf("the vendor's document publishes %s without a POST", route)
	}

	request := liveLookup(t, schema, "components", "schemas", "SystemOneRequest")
	for _, required := range []string{"state", "model", "questions"} {
		if !liveHasString(t, request, "required", required) {
			t.Errorf("SystemOneRequest does not require %q", required)
		}
	}
	if got := liveNumber(t, liveLookup(t, request, "properties", "questions"), "minProperties"); got != 1 {
		t.Errorf("SystemOneRequest.questions.minProperties = %v, want 1", got)
	}
	stateTypes := liveTypes(t, liveLookup(t, request, "properties", "state"))
	for _, want := range []string{"string", "object", "array"} {
		found := false
		for _, got := range stateTypes {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("state anyOf = %v, want it to admit %q", stateTypes, want)
		}
	}

	// The question vocabulary is closed to the three kinds this driver maps,
	// and the answers are closed to the same three: the wire has no refusal
	// kind for the driver to send back, which is why an answer outside these
	// is read as a question with no answer.
	for _, shape := range []struct{ schema, discriminator string }{
		{"Question", "Question.discriminator"},
		{"Answer", "Answer.discriminator"},
	} {
		mapping := liveLookup(t, schema, "components", "schemas", shape.schema, "discriminator", "mapping")
		for _, want := range []string{kindNoul, kindChoice, kindScore} {
			if _, ok := mapping[want]; !ok {
				t.Errorf("%s maps no %q", shape.discriminator, want)
			}
		}
		if got := len(mapping); got != 3 {
			t.Errorf("%s maps %d kinds, want 3: a fourth would be a kind this driver cannot read",
				shape.discriminator, got)
		}
	}

	// A choice carries its option map and a score its ordered levels; a noul's
	// criteria stays optional, which is why the canonical predicate lowers
	// none.
	choice := liveLookup(t, schema, "components", "schemas", "ChoiceQuestion")
	if !liveHasString(t, choice, "required", "criteria") {
		t.Error("ChoiceQuestion does not require criteria")
	}
	score := liveLookup(t, schema, "components", "schemas", "ScoreQuestion")
	if got := liveNumber(t, liveLookup(t, score, "properties", "criteria"), "minItems"); got != 1 {
		t.Errorf("ScoreQuestion.criteria.minItems = %v, want 1", got)
	}
	noul := liveLookup(t, schema, "components", "schemas", "NoulQuestion")
	if liveHasString(t, noul, "required", "criteria") {
		t.Error("NoulQuestion requires criteria; the predicate has none to lower")
	}

	// Every answer field the decoder reads is required by the vendor, and the
	// usage reports two counters and no total.
	for schemaName, fields := range map[string][]string{
		"NoulAnswer":   {"noul", "type"},
		"ChoiceAnswer": {"choice", "confidence", "probabilities", "type"},
		"ScoreAnswer":  {"score", "confidence", "legend", "probabilities", "type"},
		"Usage":        {"input_tokens", "output_tokens"},
	} {
		answer := liveLookup(t, schema, "components", "schemas", schemaName)
		for _, field := range fields {
			if !liveHasString(t, answer, "required", field) {
				t.Errorf("%s does not require %q", schemaName, field)
			}
		}
	}
}

// TestLiveAnswersADecisionRequest buys one answered request and reads it the way
// a deployment would: one state value, one question of each kind, and answers
// that must echo the caller's ids in the caller's order. It is the only lane
// that needs a credential, and it is the one that proves the wire shape end to
// end — the route accepting the compiled body is what no fixture can show.
func TestLiveAnswersADecisionRequest(t *testing.T) {
	config := liveEnabled(t)
	key := liveValue(envLiveKey)
	if key == "" {
		t.Skipf("live lane: %s is unset (environment or repo-root .env); skipping the paid call",
			envLiveKey)
	}
	ctx := context.Background()
	driver := liveDecisionDriver(t, config, key)
	request := decisionRequestFixture()

	response, err := driver.Execute(ctx, liveModelRef(t), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(response.Answers) != len(request.Questions) {
		t.Fatalf("answers = %d, want one per question", len(response.Answers))
	}
	if response.Model == "" {
		t.Error("response carries no model id")
	}
	if response.Usage.InputTokens <= 0 || response.Usage.TotalTokens !=
		response.Usage.InputTokens+response.Usage.OutputTokens {
		t.Errorf("usage = %+v, want input tokens and a total that adds up",
			response.Usage)
	}

	for index, answer := range response.Answers {
		question := request.Questions[index]
		if answer.Name != question.Name {
			t.Errorf("answer %d names %q, want the question's own name %q",
				index, answer.Name, question.Name)
		}
		switch answer.Kind {
		case inference.DecisionAnswerPredicate:
			if answer.Probability == nil ||
				*answer.Probability < 0 || *answer.Probability > 1 {
				t.Errorf("predicate answer = %+v, want a probability in [0,1]", answer)
			}
		case inference.DecisionAnswerChoice:
			if answer.Choice == nil || answer.Confidence == nil {
				t.Errorf("choice answer = %+v, want a chosen option and a confidence", answer)
			}
			if len(answer.Probabilities) != len(question.Choices) {
				t.Errorf("choice distribution = %d options, want the %d offered",
					len(answer.Probabilities), len(question.Choices))
			}
		case inference.DecisionAnswerScore:
			if answer.Score == nil || *answer.Score < 0 ||
				*answer.Score > float64(len(question.Levels)-1) {
				t.Errorf("score answer = %+v, want the value inside the levels", answer)
			}
			if len(answer.Levels) != len(question.Levels) {
				t.Errorf("score legend = %d levels, want the %d offered",
					len(answer.Levels), len(question.Levels))
			}
		case inference.DecisionAnswerRefusal:
			// The vendor publishes no refusal kind, so a refusal here means
			// the driver read an answer it could not place — which a live
			// run should never see.
			t.Errorf("answer %d came back as a refusal: %+v", index, answer)
		default:
			t.Errorf("answer %d has kind %q", index, answer.Kind)
		}
	}
	t.Logf("answered by %s: %s", response.Model, liveSummary(response))
}

// liveModelRef is the reference the lane's provider resolves: its own provider
// id, the declared model, and the lane's profile.
func liveModelRef(t *testing.T) model.ModelRef {
	t.Helper()
	name := nonEmpty(liveValue(envLiveModel), DefaultModel)
	return model.ModelRef{
		ID:      model.ModelID{Provider: liveProviderID, Name: name},
		Profile: liveProfileID,
	}
}

// liveCause renders the provider's own words out of a failed call: what the
// route answered, rather than the class the runtime wrapped around it.
func liveCause(t *testing.T, err error) string {
	t.Helper()
	var failure *inference.Error
	if !errors.As(err, &failure) {
		return err.Error()
	}
	if cause := failure.Unwrap(); cause != nil {
		return cause.Error()
	}
	return err.Error()
}

// liveSummary renders the answers of one response for the lane's log line.
func liveSummary(response inference.DecisionResponse) string {
	parts := make([]string, 0, len(response.Answers))
	for _, answer := range response.Answers {
		switch {
		case answer.Kind == inference.DecisionAnswerPredicate && answer.Probability != nil:
			parts = append(parts, fmt.Sprintf("%s=%.3f", answer.Name, *answer.Probability))
		case answer.Kind == inference.DecisionAnswerChoice && answer.Choice != nil:
			parts = append(parts, fmt.Sprintf("%s=%s", answer.Name, answer.Choice.String))
		case answer.Kind == inference.DecisionAnswerScore && answer.Score != nil:
			parts = append(parts, fmt.Sprintf("%s=%.3f", answer.Name, *answer.Score))
		default:
			parts = append(parts, fmt.Sprintf("%s=%s", answer.Name, answer.Kind))
		}
	}
	return strings.Join(parts, " ") + fmt.Sprintf(" (tokens %d+%d)",
		response.Usage.InputTokens, response.Usage.OutputTokens)
}

// liveSchema fetches and decodes the vendor's OpenAPI document from the origin
// the lane's base URL points at.
func liveSchema(t *testing.T, base string) map[string]any {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("base url %q: %v", base, err)
	}
	endpoint := parsed.Scheme + "://" + parsed.Host + liveSchemaPath
	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("build schema request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("fetch %s: %v", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fetch %s: HTTP %d", endpoint, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s: %v", endpoint, err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode %s: %v", endpoint, err)
	}
	return schema
}

// liveLookup walks a decoded schema by keys, failing the test when a key the
// vendor's document is supposed to carry is missing.
func liveLookup(t *testing.T, document map[string]any, keys ...string) map[string]any {
	t.Helper()
	current := document
	for index, key := range keys {
		next, ok := current[key]
		if !ok {
			t.Fatalf("the vendor's document has no %q", strings.Join(keys[:index+1], "."))
		}
		object, ok := next.(map[string]any)
		if !ok {
			t.Fatalf("%q is %T, want an object", strings.Join(keys[:index+1], "."), next)
		}
		current = object
	}
	return current
}

// liveHasString reports whether the array under key contains value.
func liveHasString(t *testing.T, object map[string]any, key, value string) bool {
	t.Helper()
	entries, ok := object[key].([]any)
	if !ok {
		return false
	}
	for _, entry := range entries {
		if entry == value {
			return true
		}
	}
	return false
}

// liveNumber reads one numeric field, reporting -1 when it is absent.
func liveNumber(t *testing.T, object map[string]any, key string) float64 {
	t.Helper()
	value, ok := object[key].(float64)
	if !ok {
		return -1
	}
	return value
}

// liveTypes lists the "type" each entry of an anyOf array declares.
func liveTypes(t *testing.T, object map[string]any) []string {
	t.Helper()
	entries, ok := object["anyOf"].([]any)
	if !ok {
		return nil
	}
	types := make([]string, 0, len(entries))
	for _, entry := range entries {
		if variant, ok := entry.(map[string]any); ok {
			if name, ok := variant["type"].(string); ok {
				types = append(types, name)
			}
		}
	}
	return types
}
