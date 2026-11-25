package openai

import (
	"reflect"
	"strings"
	"testing"
)

// TestDocumentCoversEverySetting is the guard this whole shape exists for.
//
// A hand-copied settings struct in a registering binary drifts silently: the fields it forgets
// still have defaults, so nothing fails — the field is just never configurable. Reflecting over
// Settings here means a field added to the implementation without a document surface (and without
// a stated reason) fails at the place the mistake was made.
func TestDocumentCoversEverySetting(t *testing.T) {
	notConfigurable := map[string]string{
		// A live *http.Client is not a value a document can carry; a deployment that needs a
		// custom transport registers its own factory rather than a YAML key.
		"HTTPClient": "a live object, not a document value",
	}
	fields := reflect.TypeOf(Settings{})
	documented := map[string]bool{}
	document := reflect.TypeOf(Document{})
	for i := range document.NumField() {
		documented[document.Field(i).Name] = true
	}
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		if documented[name] {
			continue
		}
		if reason, excused := notConfigurable[name]; excused {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("Settings.%s is excused with an empty reason", name)
			}
			continue
		}
		t.Errorf("Settings.%s has no document field: it can never be configured", name)
	}
}
