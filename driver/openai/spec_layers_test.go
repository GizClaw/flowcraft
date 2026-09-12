package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"

	"github.com/openai/openai-go/v3/responses"
)

// TestSpecValidationLayers locks the three-layer config contract: endpoint
// carries transport only, wire carries the dialect, catalog selects the model
// namespace. Cross-layer contradictions must fail at decode time.
func TestSpecValidationLayers(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "empty", raw: `{}`, ok: true},
		{name: "endpoint query and headers",
			raw: `{"endpoint":{"query":{"api-version":"2025-04-01-preview"},"headers":{"x-gw":"1"}}}`, ok: true},
		{name: "endpoint query token",
			raw: `{"endpoint":{"query":{"a&b":"1"}}}`, ok: false},
		{name: "endpoint headers reject credentials",
			raw: `{"endpoint":{"headers":{"Authorization":"Bearer x"}}}`, ok: false},
		{name: "auth header scheme",
			raw: `{"auth":{"scheme":"header","header":"Api-Key"}}`, ok: true},
		{name: "auth header needs a name",
			raw: `{"auth":{"scheme":"header"}}`, ok: false},
		{name: "auth name needs header scheme",
			raw: `{"auth":{"header":"Api-Key"}}`, ok: false},
		{name: "azure routing",
			raw: `{"endpoint":{"routing":"azure_deployment"},"auth":{"scheme":"header","header":"Api-Key"}}`, ok: true},
		{name: "azure routing defaults its own auth",
			raw: `{"endpoint":{"routing":"azure_deployment"}}`, ok: true},
		{name: "azure routing rejects bearer",
			raw: `{"endpoint":{"routing":"azure_deployment"},"auth":{"scheme":"bearer"}}`, ok: false},
		{name: "unknown routing",
			raw: `{"endpoint":{"routing":"deployment"}}`, ok: false},
		{name: "store opt in", raw: `{"wire":{"store":true}}`, ok: true},
		{name: "reasoning text channel",
			raw: `{"wire":{"reasoning_channel":"text"}}`, ok: true},
		{name: "unknown reasoning channel",
			raw: `{"wire":{"reasoning_channel":"plain"}}`, ok: false},
		{name: "text channel cannot include payload",
			raw: `{"wire":{"reasoning_channel":"text","include_reasoning_payload":true}}`, ok: false},
		{name: "declared catalog", raw: `{"catalog":"declared"}`, ok: true},
		{name: "unknown catalog", raw: `{"catalog":"builtin"}`, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeSpec(context.Background(), []byte(tc.raw))
			if tc.ok && err != nil {
				t.Fatalf("decodeSpec(%s): %v", tc.raw, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("decodeSpec(%s): want error", tc.raw)
			}
		})
	}
}

// TestResponsesStore pins the store policy: omitted keeps the driver default
// (false, so responses are not retained server-side), an explicit opt-in
// reaches the wire, and "omit" leaves the field off it entirely so a
// compatible endpoint that does not know the field is never sent one.
func TestResponsesStore(t *testing.T) {
	params := compileTextParams(t, simpleTextRequest("hi"))
	if !params.Store.Valid() || params.Store.Value {
		t.Fatalf("store = %+v, want an explicit false", params.Store)
	}

	compile := func(t *testing.T, store storePolicy) responses.ResponseNewParams {
		t.Helper()
		entry := catalog["gpt-5.6-sol"]
		entry.dialect.store = store
		compiled, err := compileResponses("gpt-5.6-sol", entry)(
			context.Background(),
			openaiModel("gpt-5.6-sol"),
			simpleTextRequest("hi"),
			inference.GenerateExecutionUnary,
		)
		if err != nil {
			t.Fatalf("compileResponses: %v", err)
		}
		return compiled.Wire.params
	}

	params = compile(t, storeEnabled)
	if !params.Store.Valid() || !params.Store.Value {
		t.Fatalf("store = %+v, want an explicit true", params.Store)
	}

	omitted := compile(t, storeOmitted)
	if omitted.Store.Valid() {
		t.Fatalf("store = %+v, want the field left off the wire", omitted.Store)
	}

	// The nil policy must come from the configured "omit", not from an
	// unset spec: the default path is covered above and still sends false.
	spec, err := decodeSpec(context.Background(), []byte(`{"wire":{"store":"omit"}}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if store := spec.store(); store != storeOmitted {
		t.Fatalf("spec.store() = %v, want storeOmitted", store)
	}
}

// TestStoreSettingDecodesBoolAndOmit pins the config surface: the field keeps
// accepting a plain boolean and gains the "omit" string.
func TestStoreSettingDecodesBoolAndOmit(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    storePolicy
		wantErr bool
	}{
		{raw: `{}`, want: storeDisabled},
		{raw: `{"wire":{"store":true}}`, want: storeEnabled},
		{raw: `{"wire":{"store":false}}`, want: storeDisabled},
		{raw: `{"wire":{"store":"omit"}}`, want: storeOmitted},
		{raw: `{"wire":{"store":"never"}}`, wantErr: true},
		{raw: `{"wire":{"store":1}}`, wantErr: true},
	} {
		spec, err := decodeSpec(context.Background(), []byte(tc.raw))
		if tc.wantErr {
			if err == nil {
				t.Errorf("decodeSpec(%s): want error", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("decodeSpec(%s): %v", tc.raw, err)
			continue
		}
		if got := spec.store(); got != tc.want {
			t.Errorf("decodeSpec(%s): store = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestCatalogDeclaredMode covers the namespace switch: the declared mode must
// not hand a third-party deployment the built-in OpenAI facts.
func TestCatalogDeclaredMode(t *testing.T) {
	declared, err := decodeSpec(context.Background(), []byte(
		`{"catalog":"declared","models":[{"name":"gpt-5.6-sol","kind":"generate",`+
			`"capabilities":{"inputs":["text"],"outputs":["text"]}}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(declared)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("declared catalog built %d models, want 1", len(models))
	}
	entry := models["gpt-5.6-sol"]
	if len(entry.capabilities.Inputs) != 1 ||
		entry.capabilities.Inputs[0] != message.PartText {
		t.Fatalf("built-in capabilities leaked into the declared catalog: %+v",
			entry.capabilities.Inputs)
	}

	inherited, err := decodeSpec(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	if _, ok := mustMerged(t, inherited)["gpt-5.6-sol"]; !ok {
		t.Fatal("builtin_declared must keep the built-in line-up")
	}
}

// TestCatalogRejectsUnsupportedInputModalities locks the promise boundary:
// publishing an input kind the OpenAI wire cannot carry fails at build time
// instead of dropping the part at request time.
func TestCatalogRejectsUnsupportedInputModalities(t *testing.T) {
	for _, kind := range []string{"video", "audio", "file"} {
		spec, err := decodeSpec(context.Background(), []byte(fmt.Sprintf(
			`{"catalog":"declared","models":[{"name":"m","kind":"generate",`+
				`"capabilities":{"inputs":["text","%s"],"outputs":["text"]}}]}`,
			kind,
		)))
		if err != nil {
			t.Fatalf("decodeSpec(%s): %v", kind, err)
		}
		if _, err := mergedCatalog(spec); err == nil {
			t.Fatalf("declaring %s input must fail the catalog build", kind)
		}
	}
}

// TestReasoningTextChannelRoundTrip covers the plain-text reasoning dialect
// end to end: the compiler carries the trace verbatim, the transport emits it
// as reasoning_text content without the encrypted-payload include, and the
// decoder reads it back.
func TestReasoningTextChannelRoundTrip(t *testing.T) {
	entry := catalog["gpt-5.6-sol"]
	entry.dialect.reasoningChannel = channelText
	entry.dialect.omitReasoningPayload = true

	request := simpleTextRequest("current")
	request.Context = []message.Message{{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.ReasoningPart{Text: "plain trace", ID: "rs_1"},
			message.TextPart{Text: "answer"},
		}},
	}}
	compiled, err := compileResponses("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileResponses: %v", err)
	}
	for _, decision := range compiled.Report.Decisions {
		if decision.Field == inference.FieldGenerateContextReasoning &&
			decision.Disposition != inference.Native {
			t.Fatalf("plain reasoning must compile native: %+v", decision)
		}
	}
	params := compiled.Wire.params
	if len(params.Include) != 0 {
		t.Fatalf("include = %v, want none on a plain-text channel", params.Include)
	}
	reasoning := params.Input.OfInputItemList[0].OfReasoning
	if reasoning == nil ||
		len(reasoning.Content) != 1 ||
		reasoning.Content[0].Text != "plain trace" ||
		len(reasoning.Summary) != 0 ||
		reasoning.EncryptedContent.Valid() {
		t.Fatalf("reasoning param = %+v", reasoning)
	}
}

// TestEndpointAuthHeaderQueryAndStaticHeaders drives a real request through a
// configured endpoint: the named header carries the key, the bearer header is
// dropped, and the configured query and static headers ride along.
func TestEndpointAuthHeaderQueryAndStaticHeaders(t *testing.T) {
	var (
		authorization string
		apiKey        string
		gateway       string
		apiVersion    string
	)
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		authorization = r.Header.Get("Authorization")
		apiKey = r.Header.Get("Api-Key")
		gateway = r.Header.Get("x-gw")
		apiVersion = r.URL.Query().Get("api-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("ok"),
		}))
	}))
	defer server.Close()

	spec, err := decodeSpec(context.Background(), []byte(fmt.Sprintf(
		`{"endpoint":{"base_url":%q,"query":{"api-version":"2025-04-01-preview"},`+
			`"headers":{"x-gw":"1"}},"auth":{"scheme":"header","header":"Api-Key"}}`,
		server.URL,
	)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	cls, err := profileMaterial{
		apiKey: resource.LiteralSecret("test-key"),
	}.newClients(context.Background(), spec)
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}
	if _, err := transportGenerate(cls.api)(
		context.Background(),
		compileTextRequest(t, simpleTextRequest("hi")),
	); err != nil {
		t.Fatalf("transportGenerate: %v", err)
	}
	if authorization != "" {
		t.Fatalf("bearer header must be dropped for a named auth header, got %q",
			authorization)
	}
	if apiKey != "test-key" {
		t.Fatalf("Api-Key = %q", apiKey)
	}
	if gateway != "1" {
		t.Fatalf("static header = %q", gateway)
	}
	if apiVersion != "2025-04-01-preview" {
		t.Fatalf("api-version = %q", apiVersion)
	}
}

func mustMerged(t *testing.T, spec Spec) map[string]catalogEntry {
	t.Helper()
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	return models
}
