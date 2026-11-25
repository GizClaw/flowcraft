package bytedance

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
)

// TestDocumentCoversEverySetting is the guard this shape exists for.
//
// A hand-copied settings struct in a registering binary drifts silently: the
// fields it forgets still have defaults, so nothing fails — the field is just
// never configurable. The guard therefore runs both ways. Forward, a [Settings]
// field without a document field (and without a stated reason) fails. Backward,
// a document field the driver's settings do not carry fails too, because such a
// field is a knob an operator writes and the driver never reads.
func TestDocumentCoversEverySetting(t *testing.T) {
	notConfigurable := map[string]string{
		// A live *http.Client is not a value a document can carry; a
		// deployment that needs a custom transport registers its own factory
		// rather than a YAML key.
		"HTTPClient": "a live object, not a document value",
		// The three below are declared on Settings but read by no code in this
		// driver, so offering them would be a knob that does nothing. Retry
		// and failover belong to the route Router, which owns the attempt
		// budget for every mode.
		"Region":         "read by no code in this driver",
		"ReasoningScope": "read by no code in this driver",
		"Retries":        "read by no code in this driver",
	}
	settings := reflect.TypeOf(Settings{})
	document := reflect.TypeOf(Document{})
	documented := map[string]bool{}
	for i := range document.NumField() {
		documented[document.Field(i).Name] = true
	}
	for i := range settings.NumField() {
		name := settings.Field(i).Name
		if documented[name] {
			continue
		}
		reason, excused := notConfigurable[name]
		if !excused {
			t.Errorf("Settings.%s has no document field: it can never be configured", name)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("Settings.%s is excused with an empty reason", name)
		}
	}
	carried := map[string]bool{}
	for i := range settings.NumField() {
		carried[settings.Field(i).Name] = true
	}
	for i := range document.NumField() {
		name := document.Field(i).Name
		if !carried[name] {
			t.Errorf("Document.%s is not a Settings field: the driver would never read it", name)
		}
	}
}

// TestDocumentDecodesDeployment reads a document the way a deployment writes
// one: the credential, the provider-specific knobs, and a model declaration
// carrying capabilities and limits.
func TestDocumentDecodesDeployment(t *testing.T) {
	document, err := decodeDocument(t, `{
		"base_url": "https://ark.example.com/api/v3",
		"api_key": "key",
		"headers": {"X-Gateway": "one"},
		"query": {"trace": "on"},
		"project": "team",
		"endpoints": {"seedance-pro": "ep-2026"},
		"timeout": "90s",
		"video_poll_interval": "2s",
		"models": [{
			"name": "seedance-pro",
			"kind": "video",
			"capabilities": {"inputs": ["text"], "modes": ["video"]},
			"limits": {"max_input_tokens": 4096},
			"video": {"seed": true, "duration_max_seconds": 12}
		}]
	}`)
	if err != nil {
		t.Fatalf("decode document: %v", err)
	}
	settings, err := document.Settings()
	if err != nil {
		t.Fatalf("resolve settings: %v", err)
	}
	if settings.APIKey != "key" {
		t.Errorf("api_key = %q, want key: the document field carries the credential", settings.APIKey)
	}
	if settings.BaseURL != "https://ark.example.com/api/v3" {
		t.Errorf("base_url = %q", settings.BaseURL)
	}
	if settings.Headers["X-Gateway"] != "one" || settings.Query["trace"] != "on" {
		t.Errorf("headers/query lost: %v %v", settings.Headers, settings.Query)
	}
	if settings.Project != "team" {
		t.Errorf("project = %q", settings.Project)
	}
	if settings.Endpoints["seedance-pro"] != "ep-2026" {
		t.Errorf("endpoints = %v", settings.Endpoints)
	}
	if settings.Timeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", settings.Timeout)
	}
	if settings.VideoPollInterval != 2*time.Second {
		t.Errorf("video_poll_interval = %v, want 2s", settings.VideoPollInterval)
	}
	if len(settings.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(settings.Models))
	}
	declared := settings.Models[0]
	if declared.Name != "seedance-pro" || declared.Kind != "video" {
		t.Errorf("model = %q/%q", declared.Name, declared.Kind)
	}
	if len(declared.Capabilities.Inputs) != 1 ||
		declared.Capabilities.Inputs[0] != message.PartText {
		t.Errorf("capabilities.inputs = %v, want [text]", declared.Capabilities.Inputs)
	}
	if declared.Limits.MaxInputTokens == nil || *declared.Limits.MaxInputTokens != 4096 {
		t.Errorf("limits.max_input_tokens = %v, want 4096", declared.Limits.MaxInputTokens)
	}
	if !declared.Video.Seed {
		t.Error("video.seed = false, want the declared true")
	}
}

// TestDocumentRejectsMisspelledKey pins the strictness the loader relies on: a
// key the document does not declare is a build error, not a silently defaulted
// setting.
func TestDocumentRejectsMisspelledKey(t *testing.T) {
	if _, err := decodeDocument(t, `{"base_urll": "https://ark.example.com"}`); err == nil {
		t.Fatal("a misspelled key was accepted")
	}
}

// decodeDocument decodes one written block strictly: a misspelled field is an
// error rather than a silently defaulted setting.
func decodeDocument(t *testing.T, written string) (Document, error) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(written))
	decoder.DisallowUnknownFields()
	var document Document
	err := decoder.Decode(&document)
	return document, err
}
