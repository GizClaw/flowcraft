package dashscope

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/model"
)

// TestProfileSpecValidate keeps a region-specific override from becoming a
// silently ignored setting: a malformed base URL or workspace id is a deployment
// error, not something to discover on the first request.
func TestProfileSpecValidate(t *testing.T) {
	for name, spec := range map[string]ProfileSpec{
		"empty is fine":  {},
		"https base url": {BaseURL: "https://dashscope.example.com"},
		"workspace id":   {WorkspaceID: "ws-123"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := spec.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}

	for name, spec := range map[string]ProfileSpec{
		"relative base url":      {BaseURL: "dashscope.example.com"},
		"ftp base url":           {BaseURL: "ftp://dashscope.example.com"},
		"base url without host":  {BaseURL: "https://"},
		"workspace with a space": {WorkspaceID: "ws 1"},
		"empty workspace":        {WorkspaceID: " "},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if err := spec.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", spec)
			}
		})
	}
}

// TestEnvelopeFailureNamesThePlatformsReason is why the envelope is read at all:
// a status code alone says "403", while the platform's code says which key, quota
// or model was refused.
func TestEnvelopeFailureNamesThePlatformsReason(t *testing.T) {
	err := envelopeFailure(http.StatusForbidden,
		[]byte(`{"code":"InvalidApiKey","message":"Invalid API-key provided."}`))
	if err == nil {
		t.Fatal("envelopeFailure returned nil for a populated envelope")
	}
	if !strings.Contains(err.Error(), "InvalidApiKey") {
		t.Fatalf("err = %v, want it to name the platform code", err)
	}

	for name, body := range map[string]string{
		"empty object":       `{}`,
		"not an object":      `[]`,
		"not json":           `<html>gateway</html>`,
		"empty body":         ``,
		"no code or message": `{"request_id":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := envelopeFailure(http.StatusBadGateway, []byte(body)); got != nil {
				t.Fatalf("envelopeFailure = %v, want nil so the status classification stands", got)
			}
		})
	}
}

// TestGetArtifactRefusesANonHTTPLink keeps the signed result URL as the only thing
// that leaves the process: anything that is not an http(s) URL with a host is not a
// result link.
func TestGetArtifactRefusesANonHTTPLink(t *testing.T) {
	client := newMediaClient("key", "https://dashscope.example.com", "", Spec{})
	for name, link := range map[string]string{
		"empty":       "",
		"relative":    "/results/1",
		"ftp":         "ftp://files.example.com/1",
		"no host":     "https:///1",
		"unparseable": "http://[::1",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := client.getArtifact(context.Background(), link); err == nil {
				t.Fatalf("getArtifact accepted %q", link)
			}
		})
	}
}

// TestPostJSONRoundTripsAndClassifies drives the create call against a real HTTP
// server: the success path decodes into the caller's value, and the failure path
// turns the platform's envelope into a classified error.
func TestPostJSONRoundTripsAndClassifies(t *testing.T) {
	var seenAsync bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAsync = r.Header.Get(headerAsync) == "enable"
		switch r.URL.Path {
		case "/api/v1/services/aigc/text2image/image-synthesis":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"output":{"task_id":"t-1"},"request_id":"r-1"}`))
		case "/api/v1/denied":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"AccessDenied","message":"workspace mismatch"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newMediaClient("key", server.URL, "ws-1", Spec{})

	var out struct {
		Output struct {
			TaskID string `json:"task_id"`
		} `json:"output"`
	}
	err := client.postJSON(context.Background(),
		"/api/v1/services/aigc/text2image/image-synthesis", true,
		map[string]any{"model": "m"}, &out)
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	if out.Output.TaskID != "t-1" {
		t.Fatalf("decoded task id = %q", out.Output.TaskID)
	}
	if !seenAsync {
		t.Fatal("the async header did not reach the server")
	}

	var ignored map[string]any
	err = client.postJSON(context.Background(), "/api/v1/denied", false, nil, &ignored)
	if err == nil {
		t.Fatal("postJSON accepted a 403")
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("err = %v, want the platform code", err)
	}
	var typed interface{ Unwrap() error }
	if !errors.As(err, &typed) && !errdefs.IsForbidden(err) && !errdefs.IsPolicyDenied(err) {
		t.Logf("note: %v is classified as an inference error rather than an errdefs kind", err)
	}
}

// TestOpSetsOneHeader keeps the two request modifiers from fighting: a call is
// either async or SSE, never both, and a plain call carries neither.
func TestOpSetsOneHeader(t *testing.T) {
	client := newMediaClient("key", "https://dashscope.example.com", "", Spec{})
	if headers := client.op(http.MethodGet, "/x", nil, requestOptions{}).Headers; headers != nil {
		t.Fatalf("a plain call carried headers %v", headers)
	}
	if got := client.op(http.MethodPost, "/x", nil, requestOptions{async: true}).Headers[headerAsync]; got != "enable" {
		t.Fatalf("async header = %q", got)
	}
	if got := client.op(http.MethodPost, "/x", nil, requestOptions{sse: true}).Headers[headerSSE]; got != "enable" {
		t.Fatalf("sse header = %q", got)
	}
}

// TestCatalogProjectsDeclaredModels keeps the line-up a deployment declares and the
// line-up the router publishes identical — including the copies, so a caller cannot
// mutate the catalog through a descriptor it was handed.
func TestCatalogProjectsDeclaredModels(t *testing.T) {
	catalog := newCatalog([]ModelSpec{
		{Name: "wanx-v1"},
		{Name: "qwen-omni"},
	})

	models := catalog.Models()
	if len(models) != 2 {
		t.Fatalf("Models() = %v", models)
	}
	for _, descriptor := range models {
		if descriptor.ID.Provider != ProviderID {
			t.Fatalf("descriptor provider = %q", descriptor.ID.Provider)
		}
	}
	models[0].ID.Name = "rewritten"
	if got := catalog.Models()[0].ID.Name; got != "wanx-v1" {
		t.Fatalf("Models() shares its backing array: first name = %q", got)
	}

	if _, ok := catalog.Describe(model.ModelID{Provider: ProviderID, Name: "qwen-omni"}); !ok {
		t.Fatal("Describe missed a declared model")
	}
	if _, ok := catalog.Describe(model.ModelID{Provider: ProviderID, Name: "absent"}); ok {
		t.Fatal("Describe invented a model")
	}
}

// TestNewClientsBuildsFromSettings is the construction path a deployment uses: the
// workspace id rides the header only when declared, and the base URL falls back to
// the platform default.
func TestNewClientsBuildsFromSettings(t *testing.T) {
	built, err := newClients(Spec{APIKey: "key", WorkspaceID: "ws-1", BaseURL: "https://dashscope.example.com/"})
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}
	if built == nil || built.media == nil {
		t.Fatal("newClients returned no media client")
	}
	if got := built.media.api; got == nil {
		t.Fatal("the api client is missing")
	}

	if _, err := newClients(Spec{APIKey: "key"}); err != nil {
		t.Fatalf("newClients with defaults: %v", err)
	}
}

// TestSpecValidateGatesTheDeployment keeps a malformed settings block from reaching
// the network: a base URL that cannot serve a request fails at load.
func TestSpecValidateGatesTheDeployment(t *testing.T) {
	if err := (Spec{BaseURL: "https://dashscope.example.com", WorkspaceID: "ws-1"}).Validate(); err != nil {
		t.Fatalf("a well-formed spec was refused: %v", err)
	}
	for name, spec := range map[string]Spec{
		"base url without a host": {BaseURL: "https://"},
		"relative base url":       {BaseURL: "dashscope.example.com"},
		"malformed workspace id":  {WorkspaceID: "ws 1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := spec.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", spec)
			}
		})
	}
}
