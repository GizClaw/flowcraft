package inference

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

func TestIntentOutputKinds(t *testing.T) {
	if kinds := (Intent{}).OutputKinds(); len(kinds) != 0 {
		t.Fatalf("empty intent kinds = %v, want none", kinds)
	}
	if kinds := (Intent{Text: &TextIntent{}}).OutputKinds(); !reflect.DeepEqual(
		kinds,
		[]message.PartKind{message.PartText},
	) {
		t.Fatalf("text intent kinds = %v", kinds)
	}
	all := Intent{
		Text:  &TextIntent{},
		Image: &ImageIntent{},
		Audio: &AudioIntent{},
		Video: &VideoIntent{},
	}
	want := []message.PartKind{
		message.PartText,
		message.PartImage,
		message.PartAudio,
		message.PartVideo,
	}
	if kinds := all.OutputKinds(); !reflect.DeepEqual(kinds, want) {
		t.Fatalf("all intent kinds = %v, want %v", kinds, want)
	}
}

func TestValidateGenerateText(t *testing.T) {
	arraySchema := json.RawMessage(
		`{"type":"array","items":{"type":"string"}}`,
	)
	cases := []struct {
		name    string
		output  string
		format  *ResponseFormat
		wantErr string
	}{
		{
			name:   "no format skips validation",
			output: `["a","b"]`,
		},
		{
			name:   "text format skips validation",
			output: `["a","b"]`,
			format: &ResponseFormat{Kind: ResponseText},
		},
		{
			name:   "json_object accepts object",
			output: `{"answer":"42"}`,
			format: &ResponseFormat{Kind: ResponseJSONObject},
		},
		{
			name:    "json_object rejects array",
			output:  `["a","b"]`,
			format:  &ResponseFormat{Kind: ResponseJSONObject},
			wantErr: "structured generate response must be a JSON object",
		},
		{
			name:   "json_schema accepts array root",
			output: `["a","b"]`,
			format: &ResponseFormat{
				Kind:   ResponseJSONSchema,
				Name:   "answer",
				Schema: arraySchema,
			},
		},
		{
			name:    "json_schema array root rejects object",
			output:  `{"a":"b"}`,
			format:  &ResponseFormat{Kind: ResponseJSONSchema, Name: "answer", Schema: arraySchema},
			wantErr: "does not match requested JSON schema",
		},
		{
			name:   "json_schema rejects non-conforming object",
			output: `{"answer":42}`,
			format: &ResponseFormat{
				Kind: ResponseJSONSchema,
				Name: "answer",
				Schema: json.RawMessage(
					`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`,
				),
			},
			wantErr: "does not match requested JSON schema",
		},
		{
			name:    "json_schema rejects invalid JSON",
			output:  "not json",
			format:  &ResponseFormat{Kind: ResponseJSONSchema, Name: "answer", Schema: arraySchema},
			wantErr: "structured generate response is not valid JSON",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.format != nil {
				if err := tc.format.Validate(); err != nil {
					t.Fatalf("format Validate: %v", err)
				}
			}
			err := validateGenerateText(tc.output, tc.format)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateGenerateText = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateGenerateText error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
