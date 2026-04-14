package llm

import "testing"

func TestApplyOptions_Defaults(t *testing.T) {
	opts := ApplyOptions()
	if opts.Temperature != nil {
		t.Fatal("Temperature should be nil by default")
	}
	if opts.MaxTokens != nil {
		t.Fatal("MaxTokens should be nil by default")
	}
	if opts.Tools != nil {
		t.Fatal("Tools should be nil by default")
	}
}

func TestApplyOptions_All(t *testing.T) {
	opts := ApplyOptions(
		WithTemperature(0.7),
		WithMaxTokens(1024),
		WithTopP(0.9),
		WithTopK(50),
		WithStopWords("stop1", "stop2"),
		WithFrequencyPenalty(0.5),
		WithPresencePenalty(0.3),
		WithJSONMode(true),
		WithTools(ToolDefinition{Name: "test", Description: "test tool"}),
		WithToolChoiceAuto(),
	)

	if opts.Temperature == nil || *opts.Temperature != 0.7 {
		t.Fatalf("Temperature = %v, want 0.7", opts.Temperature)
	}
	if opts.MaxTokens == nil || *opts.MaxTokens != 1024 {
		t.Fatalf("MaxTokens = %v, want 1024", opts.MaxTokens)
	}
	if opts.TopP == nil || *opts.TopP != 0.9 {
		t.Fatalf("TopP = %v, want 0.9", opts.TopP)
	}
	if opts.TopK == nil || *opts.TopK != 50 {
		t.Fatalf("TopK = %v, want 50", opts.TopK)
	}
	if len(opts.StopWords) != 2 {
		t.Fatalf("StopWords len = %d, want 2", len(opts.StopWords))
	}
	if opts.JSONMode == nil || !*opts.JSONMode {
		t.Fatal("JSONMode should be true")
	}
	if len(opts.Tools) != 1 {
		t.Fatalf("Tools len = %d, want 1", len(opts.Tools))
	}
	if opts.ToolChoice == nil || opts.ToolChoice.Type != ToolChoiceAuto {
		t.Fatal("ToolChoice should be auto")
	}
}

func TestToolChoiceOptions(t *testing.T) {
	tests := []struct {
		name string
		opt  GenerateOption
		want ToolChoiceType
	}{
		{"auto", WithToolChoiceAuto(), ToolChoiceAuto},
		{"none", WithToolChoiceNone(), ToolChoiceNone},
		{"required", WithToolChoiceRequired(), ToolChoiceRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := ApplyOptions(tt.opt)
			if opts.ToolChoice == nil || opts.ToolChoice.Type != tt.want {
				t.Fatalf("ToolChoice.Type = %v, want %v", opts.ToolChoice, tt.want)
			}
		})
	}
}

func TestWithToolChoiceSpecific(t *testing.T) {
	opts := ApplyOptions(WithToolChoiceSpecific("my_tool"))
	if opts.ToolChoice == nil {
		t.Fatal("ToolChoice should not be nil")
	}
	if opts.ToolChoice.Type != ToolChoiceSpecific {
		t.Fatalf("Type = %v, want specific", opts.ToolChoice.Type)
	}
	if opts.ToolChoice.Name != "my_tool" {
		t.Fatalf("Name = %q, want %q", opts.ToolChoice.Name, "my_tool")
	}
}

func TestWithThinking(t *testing.T) {
	opts := ApplyOptions(WithThinking(true))
	if opts.Thinking == nil || !*opts.Thinking {
		t.Fatal("Thinking should be true")
	}
	opts = ApplyOptions(WithThinking(false))
	if opts.Thinking == nil || *opts.Thinking {
		t.Fatal("Thinking should be false")
	}
}

func TestWithExtra(t *testing.T) {
	opts := ApplyOptions(WithExtra("key1", "val1"), WithExtra("key2", 42))
	if opts.Extra == nil {
		t.Fatal("Extra should not be nil")
	}
	if opts.Extra["key1"] != "val1" {
		t.Fatalf("Extra[key1] = %v, want val1", opts.Extra["key1"])
	}
	if opts.Extra["key2"] != 42 {
		t.Fatalf("Extra[key2] = %v, want 42", opts.Extra["key2"])
	}
}

func TestWithJSONSchema(t *testing.T) {
	schema := JSONSchemaParam{Name: "test", Description: "desc", Strict: true}
	opts := ApplyOptions(WithJSONSchema(schema))
	if opts.JSONSchema == nil {
		t.Fatal("JSONSchema should not be nil")
	}
	if opts.JSONSchema.Name != "test" || !opts.JSONSchema.Strict {
		t.Fatalf("JSONSchema = %+v", opts.JSONSchema)
	}
}
