package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"
)

// TestConfiguredModelsExistInCatalog guards the failure that cost us a whole
// round of experiments: the deploy asked for `deepseek-chat`, the endpoint does
// not serve it, and every stage was silently served `deepseek-flash` instead.
// A model name that is not in the provider's catalog is a configuration error,
// not something to discover from a fingerprint months later.
//
// It reads deploy.yaml, so it covers both of the document's providers: the
// gateway publishes one catalog for the two wire surfaces, which is why the
// generate, decision, and embedding models are all listed by one GET /v1/models.
// A name missing from it is not substituted: the gateway answers 403
// service_model_unavailable, and the run stops where it started.
func TestConfiguredModelsExistInCatalog(t *testing.T) {
	if os.Getenv("MEMORY_EVAL_LIVE") != "1" {
		t.Skip("set MEMORY_EVAL_LIVE=1 to check the provider catalogs")
	}
	loadEnvFile(liveEnvFile(t))
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	providers := []struct {
		name    string
		keyEnv  string
		url     string
		pattern string
	}{
		{
			name:    "gateway",
			keyEnv:  "GATEWAY_API_KEY",
			url:     "https://ai.haivivi.cn/v1/models",
			pattern: `name:\s*((?:deepseek|doubao|azure|glm|minimax)-[a-z0-9.\-]+)`,
		},
	}

	for _, provider := range providers {
		t.Run(provider.name, func(t *testing.T) {
			key := os.Getenv(provider.keyEnv)
			if key == "" {
				t.Skipf("%s is required", provider.keyEnv)
			}
			names := declaredModels(t, raw, provider.pattern, provider.name)
			if len(names) == 0 {
				t.Fatalf("deploy.yaml declares no %s models", provider.name)
			}
			served := servedModels(t, provider.url, key)
			for _, name := range names {
				_, ok := served[name]
				if !ok {
					t.Fatalf("deploy.yaml asks for %q, which the endpoint does not serve (available: %v); "+
						"an unknown name is silently substituted, so results would not come from the configured model",
						name, slices.Sorted(maps.Keys(served)))
				}
			}
			t.Logf("configured %s models %v all exist in the catalog", provider.name, names)
		})
	}
}

// declaredModels reads the model names the deploy document declares. The
// pattern is anchored on a name prefix so the prose in the document's comments
// is not mistaken for a declaration.
func declaredModels(t *testing.T, document []byte, pattern, provider string) []string {
	t.Helper()
	names := map[string]struct{}{}
	for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(string(document), -1) {
		names[match[1]] = struct{}{}
	}
	return slices.Sorted(maps.Keys(names))
}

// servedModels returns every model the endpoint lists.
func servedModels(t *testing.T, url, key string) map[string]struct{} {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, response.Status)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) == 0 {
		t.Fatalf("GET %s listed no models", url)
	}
	served := make(map[string]struct{}, len(payload.Data))
	for _, model := range payload.Data {
		served[model.ID] = struct{}{}
	}
	return served
}
