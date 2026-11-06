package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

// chatBody is the smallest answer the chat surface accepts.
const chatBody = `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

// A profile must reach the wire: two profiles in one deployment authenticate
// with different keys, resolved once each.
func TestAPIKeyLookupResolvesPerProfile(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatBody)
	}))
	defer server.Close()

	lookups := map[string]int{}
	provider, err := New(Settings{
		API:     APIChat,
		BaseURL: server.URL,
		APIKeyLookup: func(profile string) (string, error) {
			lookups[profile]++
			switch profile {
			case "alpha":
				return "key-alpha", nil
			case "beta":
				return "key-beta", nil
			default:
				return "", errors.New("unknown profile " + profile)
			}
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	for _, profile := range []string{"alpha", "beta", "alpha"} {
		request := baseRequest(t)
		request.Model.Profile = profile
		if _, err := provider.Generate(context.Background(), request); err != nil {
			t.Fatalf("Generate(%s): %v", profile, err)
		}
	}

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	want := []string{"Bearer key-alpha", "Bearer key-beta", "Bearer key-alpha"}
	if len(got) != len(want) {
		t.Fatalf("saw %d calls, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d authenticated as %q, want %q", i, got[i], want[i])
		}
	}
	if lookups["alpha"] != 2 || lookups["beta"] != 1 {
		t.Errorf("lookups = %v, want one per call: nothing is cached", lookups)
	}
}

// A key that rotates between calls must take effect: the lookup runs per call
// precisely so a rotated credential is not pinned behind a cached client.
func TestAPIKeyLookupSeesRotation(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatBody)
	}))
	defer server.Close()

	keys := []string{"key-one", "key-two"}
	var n int
	provider, err := New(Settings{
		API:     APIChat,
		BaseURL: server.URL,
		APIKeyLookup: func(string) (string, error) {
			key := keys[n%len(keys)]
			n++
			return key, nil
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for i := 0; i < 2; i++ {
		request := baseRequest(t)
		request.Model.Profile = "rotating"
		if _, err := provider.Generate(context.Background(), request); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "Bearer key-one" || got[1] != "Bearer key-two" {
		t.Errorf("authentications = %v, want the rotated keys in order", got)
	}
}

// A lookup that fails is a configuration problem at the call, not a provider
// failure: the kind says the request named something the deployment cannot serve.
func TestAPIKeyLookupFailureIsInvalidRequest(t *testing.T) {
	provider, err := New(Settings{
		API:     APIChat,
		BaseURL: "http://127.0.0.1:1",
		APIKeyLookup: func(string) (string, error) {
			return "", errors.New("no such profile")
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	request := baseRequest(t)
	request.Model.Profile = "missing"
	_, err = provider.Generate(context.Background(), request)
	if !inference.IsKind(err, inference.InvalidRequest) {
		t.Errorf("kind = %v, want InvalidRequest", err)
	}
}
