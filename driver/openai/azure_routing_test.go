package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/resource"
)

// The Azure deployment mode owns three things the plain OpenAI routes do not
// have: the Api-Key header, the api-version query, and the
// /openai/deployments/{model}/… path rewriting. These tests drive real
// requests through a fake Azure resource so a future SDK bump cannot change
// the first two silently, and so the routes the SDK rewrites (and the ones it
// does not) are pinned rather than assumed.

// azureClients builds the transport for one Azure-routed endpoint.
func azureClients(t *testing.T, server *httptest.Server, extra string) *clients {
	t.Helper()
	spec, err := decodeSpec(context.Background(), []byte(fmt.Sprintf(
		`{"endpoint":{"base_url":%q,"routing":"azure_deployment"%s}}`,
		server.URL, extra,
	)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	cls, err := profileMaterial{
		apiKey: resource.LiteralSecret("azure-key"),
	}.newClients(context.Background(), spec)
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}
	return cls
}

// azureRequest records what reached the fake resource.
type azureRequest struct {
	path       string
	apiVersion string
	apiKey     string
	bearer     string
}

func newAzureResource(
	t *testing.T,
	respond func(w http.ResponseWriter, r *http.Request),
) (*httptest.Server, *[]azureRequest) {
	t.Helper()
	seen := &[]azureRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		*seen = append(*seen, azureRequest{
			path:       r.URL.Path,
			apiVersion: r.URL.Query().Get("api-version"),
			apiKey:     r.Header.Get("Api-Key"),
			bearer:     r.Header.Get("Authorization"),
		})
		respond(w, r)
	}))
	return server, seen
}

func (r azureRequest) assertCredentials(t *testing.T) {
	t.Helper()
	if r.apiKey != "azure-key" {
		t.Errorf("Api-Key = %q, want the profile key", r.apiKey)
	}
	if r.bearer != "" {
		t.Errorf("Authorization = %q, want no bearer header in azure mode", r.bearer)
	}
	if r.apiVersion != DefaultAzureAPIVersion {
		t.Errorf("api-version = %q, want %q", r.apiVersion, DefaultAzureAPIVersion)
	}
}

// TestAzureRoutingChatRewritesDeploymentPath pins the deployment rewrite for
// the Chat Completions route the SDK knows about: the deployment id comes from
// the compiled model name, not from the base URL.
func TestAzureRoutingChatRewritesDeploymentPath(t *testing.T) {
	server, seen := newAzureResource(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": "chatcmpl_1",
			"object": "chat.completion",
			"choices": [{"index": 0,
				"message": {"role": "assistant", "content": "ok"},
				"finish_reason": "stop"}]
		}`)
	})
	defer server.Close()

	wire := chatWire()
	request, err := compileChatFor(
		"gpt-5.6-sol", testTargetWith("gpt-5.6-sol", wire),
	)(context.Background(), openaiModel("gpt-5.6-sol"), simpleTextRequest("hi"), inference.GenerateExecutionUnary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transportChatGenerate(azureClients(t, server, "").api)(
		context.Background(), request.Wire,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	got := (*seen)[0]
	got.assertCredentials(t)
	if want := "/openai/deployments/gpt-5.6-sol/chat/completions"; got.path != want {
		t.Fatalf("path = %q, want %q", got.path, want)
	}
}

// TestAzureRoutingResponsesPath pins what the Azure mode does with the
// Responses surface: the SDK's rewrite table covers the legacy JSON and
// multipart routes only, so /responses is prefixed with /openai and the
// deployment id is NOT injected. The behavior is pinned here because it is a
// property of the endpoint mode the migration path tells deployments to use,
// not an implementation detail.
func TestAzureRoutingResponsesPath(t *testing.T) {
	server, seen := newAzureResource(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("ok"),
		}))
	})
	defer server.Close()

	entry := declarations["gpt-5.6-sol"]
	compiled, err := compileResponsesFor("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		simpleTextRequest("hi"),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transportGenerate(azureClients(t, server, "").api)(
		context.Background(), compiled.Wire,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	got := (*seen)[0]
	got.assertCredentials(t)
	if got.path != "/openai/responses" {
		t.Fatalf("path = %q, want /openai/responses", got.path)
	}
}

// TestAzureRoutingImageEditRewritesDeploymentPath pins the multipart branch:
// the deployment id is read out of the multipart body, so the rewrite works
// for image edits as well as JSON routes.
func TestAzureRoutingImageEditRewritesDeploymentPath(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	)
	if err != nil {
		t.Fatal(err)
	}
	server, seen := newAzureResource(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"data": []map[string]any{
				{"b64_json": base64.StdEncoding.EncodeToString(png)},
			},
		})
		_, _ = fmt.Fprint(w, string(payload))
	})
	defer server.Close()

	source, err := media.NewImageBytes(png, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	compiled, err := compileImage("gpt-image-2")(
		context.Background(),
		openaiModel("gpt-image-2"),
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "make it a red circle"},
						message.ImagePart{Source: source},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{}},
				},
			},
		},
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileImage: %v", err)
	}
	if _, err := transportImage(azureClients(t, server, "").api)(
		context.Background(), compiled.Wire,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	got := (*seen)[0]
	got.assertCredentials(t)
	if want := "/openai/deployments/gpt-image-2/images/edits"; got.path != want {
		t.Fatalf("path = %q, want %q", got.path, want)
	}
}

// TestAzureRoutingHonorsConfiguredAPIVersion pins the override path the
// migration note advertises (endpoint.query["api-version"]).
func TestAzureRoutingHonorsConfiguredAPIVersion(t *testing.T) {
	server, seen := newAzureResource(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": "chatcmpl_1",
			"object": "chat.completion",
			"choices": [{"index": 0,
				"message": {"role": "assistant", "content": "ok"},
				"finish_reason": "stop"}]
		}`)
	})
	defer server.Close()

	wire := chatWire()
	request, err := compileChatFor(
		"gpt-5.6-sol", testTargetWith("gpt-5.6-sol", wire),
	)(context.Background(), openaiModel("gpt-5.6-sol"), simpleTextRequest("hi"), inference.GenerateExecutionUnary)
	if err != nil {
		t.Fatal(err)
	}
	cls := azureClients(t, server, `,"query":{"api-version":"2026-01-01-preview"}`)
	if _, err := transportChatGenerate(cls.api)(context.Background(), request.Wire); err != nil {
		t.Fatalf("transport: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(*seen))
	}
	if got := (*seen)[0].apiVersion; got != "2026-01-01-preview" {
		t.Fatalf("api-version = %q, want the configured override", got)
	}
}

// TestAzureRoutingRequiresHTTPSForRemoteEndpoints pins the security half of
// the loopback opt-in: plaintext HTTP is tolerated only for a loopback base
// URL, and the SDK still refuses to send the credential anywhere else.
func TestAzureRoutingRequiresHTTPSForRemoteEndpoints(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"endpoint":{"base_url":"http://azure.invalid/v1",`+
			`"routing":"azure_deployment"}}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	cls, err := profileMaterial{
		apiKey: resource.LiteralSecret("azure-key"),
	}.newClients(context.Background(), spec)
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}
	request, err := compileChatFor(
		"gpt-5.6-sol", testTargetWith("gpt-5.6-sol", chatWire()),
	)(context.Background(), openaiModel("gpt-5.6-sol"), simpleTextRequest("hi"), inference.GenerateExecutionUnary)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transportChatGenerate(cls.api)(context.Background(), request.Wire)
	if err == nil || !strings.Contains(err.Error(), "require HTTPS") {
		t.Fatalf("transport error = %v, want the SDK's HTTPS requirement", err)
	}
}

// TestLoopbackHTTP pins which base URLs may take the SDK's unsafe-HTTP
// opt-in: plaintext HTTP on localhost or a literal loopback IP.
func TestLoopbackHTTP(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{url: "http://127.0.0.1:8080/v1", want: true},
		{url: "http://localhost:8080", want: true},
		{url: "http://[::1]:8080/v1", want: true},
		{url: "https://127.0.0.1:8443", want: false},
		{url: "http://azure.invalid/v1", want: false},
		{url: "http://10.0.0.7/v1", want: false},
		{url: "://missing-scheme", want: false},
	} {
		if got := loopbackHTTP(tc.url); got != tc.want {
			t.Errorf("loopbackHTTP(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
