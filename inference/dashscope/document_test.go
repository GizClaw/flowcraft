package dashscope

import (
	"encoding/json"
	"github.com/GizClaw/flowcraft/core/model"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDocumentCoversEverySetting is the guard this shape exists for.
//
// A hand-copied settings struct in a registering binary drifts silently: the
// fields it forgets still have defaults, so nothing fails — the field is just
// never configurable. The guard therefore runs both ways. Forward, a [Spec]
// field without a document field (and without a stated reason) fails. Backward,
// a document field the driver's spec does not carry fails too, because such a
// field is a knob an operator writes and the driver never reads.
func TestDocumentCoversEverySetting(t *testing.T) {
	notConfigurable := map[string]string{
		// A live *http.Client is not a value a document can carry; a
		// deployment that needs a custom transport registers its own factory
		// rather than a YAML key.
		"HTTPClient": "a live object, not a document value",
		// Declared on Spec but read by no code in this driver, so offering it
		// would be a knob that does nothing.
		"ReasoningScope": "read by no code in this driver",
	}
	settings := reflect.TypeOf(Spec{})
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
			t.Errorf("Spec.%s has no document field: it can never be configured", name)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("Spec.%s is excused with an empty reason", name)
		}
	}
	carried := map[string]bool{}
	for i := range settings.NumField() {
		carried[settings.Field(i).Name] = true
	}
	for i := range document.NumField() {
		name := document.Field(i).Name
		if !carried[name] {
			t.Errorf("Document.%s is not a Spec field: the driver would never read it", name)
		}
	}
}

// TestDocumentDecodesDeployment reads a document the way a deployment writes
// one: the credential, the provider-specific knobs, and a model declaration
// carrying capabilities and limits.
func TestDocumentDecodesDeployment(t *testing.T) {
	document, err := decodeDocument(t, `{
		"base_url": "https://workspace.cn-beijing.maas.aliyuncs.com",
		"api_key": "key",
		"workspace_id": "workspace",
		"poll_interval_millis": 2000,
		"timeout": "3m",
		"models": [{
			"name": "wan2.7-t2v",
			"kind": "video",
			"wire_model": "wan2.7-t2v-2026-06-12",
			"capabilities": {"inputs": ["text"], "modes": ["video"]},
			"limits": {"max_input_tokens": 2048},
			"video": {"input": "t2v", "resolutions": ["720P"], "ratios": ["16:9"]}
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
	if settings.BaseURL != "https://workspace.cn-beijing.maas.aliyuncs.com" {
		t.Errorf("base_url = %q", settings.BaseURL)
	}
	if settings.WorkspaceID != "workspace" {
		t.Errorf("workspace_id = %q", settings.WorkspaceID)
	}
	if settings.PollIntervalMillis != 2000 || settings.pollInterval() != 2*time.Second {
		t.Errorf("poll_interval_millis = %d (%v)", settings.PollIntervalMillis, settings.pollInterval())
	}
	if settings.Timeout != 3*time.Minute {
		t.Errorf("timeout = %v, want 3m", settings.Timeout)
	}
	if settings.timeout() != 3*time.Minute {
		t.Errorf("the media client would get %v, want 3m", settings.timeout())
	}
	if len(settings.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(settings.Models))
	}
	declared := settings.Models[0]
	if declared.Name != "wan2.7-t2v" || declared.Kind != "video" {
		t.Errorf("model = %q/%q", declared.Name, declared.Kind)
	}
	if declared.WireModel != "wan2.7-t2v-2026-06-12" {
		t.Errorf("wire_model = %q", declared.WireModel)
	}
	if len(declared.Capabilities.Modes) != 1 ||
		declared.Capabilities.Modes[0] != model.ModeVideo {
		t.Errorf("capabilities.modes = %v, want [video]", declared.Capabilities.Modes)
	}
	if declared.Limits.MaxInputTokens == nil || *declared.Limits.MaxInputTokens != 2048 {
		t.Errorf("limits.max_input_tokens = %v, want 2048", declared.Limits.MaxInputTokens)
	}
	if declared.Video.Input != "t2v" || len(declared.Video.Resolutions) != 1 {
		t.Errorf("video = %+v, want the declared t2v surface", declared.Video)
	}
}

// TestDocumentRejectsMisspelledKey pins the strictness the loader relies on: a
// key the document does not declare is a build error, not a silently defaulted
// setting.
func TestDocumentRejectsMisspelledKey(t *testing.T) {
	if _, err := decodeDocument(t, `{"workspace_idd": "workspace"}`); err == nil {
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
