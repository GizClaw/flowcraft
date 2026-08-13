package utils

import (
	"encoding/json"
	"strings"
	"testing"
)

type testDoc struct {
	Version   string `json:"version"`
	MaxRevise int    `json:"max_revise"`
}

func TestFormatOf(t *testing.T) {
	tests := []struct {
		name string
		data string
		want Format
	}{
		{name: "json object", data: `{"version":"v1"}`, want: FormatJSON},
		{name: "json object with leading whitespace", data: " \t\n{\"a\":1}", want: FormatJSON},
		{name: "yaml mapping", data: "version: v1\n", want: FormatYAML},
		{name: "yaml list", data: "- a\n- b\n", want: FormatYAML},
		{name: "json array is yaml by k8s rule", data: `[1,2]`, want: FormatYAML},
		{name: "empty document", data: "", want: FormatYAML},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatOf([]byte(tt.data)); got != tt.want {
				t.Fatalf("FormatOf(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestToJSON_YAML(t *testing.T) {
	data, err := ToJSON([]byte("version: v1\nmax_revise: 2\n"))
	if err != nil {
		t.Fatalf("ToJSON error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("converted output is not JSON: %v", err)
	}
	if decoded["version"] != "v1" || decoded["max_revise"] != float64(2) {
		t.Fatalf("converted = %v", decoded)
	}
}

func TestToJSON_JSONPassthrough(t *testing.T) {
	input := []byte(`{"version":"v1"}`)
	data, err := ToJSON(input)
	if err != nil {
		t.Fatalf("ToJSON error: %v", err)
	}
	if string(data) != string(input) {
		t.Fatalf("ToJSON = %q, want passthrough %q", data, input)
	}
}

func TestToJSON_RejectsInvalidJSON(t *testing.T) {
	if _, err := ToJSON([]byte(`{"version":`)); err == nil {
		t.Fatal("ToJSON accepted an invalid JSON document")
	}
}

func TestDecode_YAML(t *testing.T) {
	doc, err := Decode[testDoc]([]byte("version: v1\nmax_revise: 2\n"))
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if doc.Version != "v1" || doc.MaxRevise != 2 {
		t.Fatalf("Decode = %+v", doc)
	}
}

func TestDecode_JSON(t *testing.T) {
	doc, err := Decode[testDoc]([]byte(`{"version":"v1","max_revise":2}`))
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if doc.Version != "v1" || doc.MaxRevise != 2 {
		t.Fatalf("Decode = %+v", doc)
	}
}

func TestDecode_RejectsUnknownFields(t *testing.T) {
	for _, data := range []string{
		`{"version":"v1","unknown":1}`,
		"version: v1\nunknown: 1\n",
	} {
		if _, err := Decode[testDoc]([]byte(data)); err == nil {
			t.Fatalf("Decode accepted unknown field in %q", data)
		} else if !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("Decode error = %v, want unknown-field error", err)
		}
	}
}

func TestDecode_RejectsTrailingDocument(t *testing.T) {
	if _, err := Decode[testDoc]([]byte(`{"version":"v1"} {"version":"v2"}`)); err == nil {
		t.Fatal("Decode accepted multiple JSON documents")
	}
	if _, err := Decode[testDoc]([]byte("version: v1\n---\nversion: v2\n")); err == nil {
		t.Fatal("Decode accepted multiple YAML documents")
	}
}
