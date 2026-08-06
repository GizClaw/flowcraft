package inference

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestUsageValidatesProviderShapes(t *testing.T) {
	t.Run("OpenAI cache counters may overlap", func(t *testing.T) {
		usage := Usage{
			InputTokens:  4_583,
			OutputTokens: 300,
			TotalTokens:  4_883,
			Input: InputTokenUsage{
				CacheReadTokens:  count(3_945),
				CacheWriteTokens: count(4_580),
			},
			Output: OutputTokenUsage{
				ReasoningTokens:     count(120),
				ReasoningAccounting: ReasoningIncludedInOutput,
			},
		}
		if err := usage.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("Anthropic cache creation has TTL breakdown", func(t *testing.T) {
		usage := Usage{
			InputTokens:  2_095 + 2_051 + 2_051,
			OutputTokens: 503,
			TotalTokens:  2_095 + 2_051 + 2_051 + 503,
			Input: InputTokenUsage{
				CacheReadTokens:  count(2_051),
				CacheWriteTokens: count(2_051),
				UncachedTokens:   count(2_095),
				CacheWrites: []CacheWriteUsage{
					{TTL: CacheTTL5Minutes, Tokens: 2_051},
				},
			},
			Output: OutputTokenUsage{
				ReasoningTokens:     count(200),
				ReasoningAccounting: ReasoningIncludedInOutput,
			},
			Tools: []ToolUsage{{Kind: ToolUsageWebFetch, Requests: 2}},
		}
		if err := usage.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("Perplexity auxiliary reasoning is outside output total", func(t *testing.T) {
		usage := Usage{
			InputTokens:  33,
			OutputTokens: 11_395,
			TotalTokens:  11_428,
			Output: OutputTokenUsage{
				ReasoningTokens:     count(193_947),
				ReasoningAccounting: ReasoningAdditional,
			},
			Auxiliary: []AuxiliaryTokenUsage{{
				Kind: AuxiliaryCitationTokens, Tokens: 19_028,
			}},
			Tools: []ToolUsage{{Kind: ToolUsageSearchQuery, Requests: 21}},
		}
		if err := usage.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("multimodal and exact billing details", func(t *testing.T) {
		usage := Usage{
			InputTokens:  200,
			OutputTokens: 10,
			TotalTokens:  210,
			Input: InputTokenUsage{ByModality: []ModalityTokenUsage{
				{Modality: ModalityText, Tokens: 50},
				{Modality: ModalityImage, Tokens: 150},
			}},
			Billing: &BillingUsage{
				InputTokens:  count(180),
				OutputTokens: count(10),
				Cost: &Money{
					Currency: "USD",
					Units:    1_585_000,
					Scale:    10,
				},
			},
		}
		if err := usage.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
}

func TestUsageRejectsInvalidBreakdowns(t *testing.T) {
	tests := []struct {
		name  string
		usage Usage
	}{
		{
			name:  "incorrect total",
			usage: Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 4},
		},
		{
			name: "duplicate modality",
			usage: Usage{
				Input: InputTokenUsage{ByModality: []ModalityTokenUsage{
					{Modality: ModalityText},
					{Modality: ModalityText},
				}},
			},
		},
		{
			name: "reasoning inclusion exceeds output",
			usage: Usage{
				OutputTokens: 1,
				TotalTokens:  1,
				Output: OutputTokenUsage{
					ReasoningTokens:     count(2),
					ReasoningAccounting: ReasoningIncludedInOutput,
				},
			},
		},
		{
			name: "cache TTL breakdown mismatch",
			usage: Usage{
				Input: InputTokenUsage{
					CacheWriteTokens: count(2),
					CacheWrites: []CacheWriteUsage{
						{TTL: CacheTTL5Minutes, Tokens: 1},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.usage.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestOptionalUsageCounterPreservesReportedZero(t *testing.T) {
	usage := Usage{
		Input:               InputTokenUsage{CacheReadTokens: count(0)},
		GeneratedImages:     count(0),
		InputCharacters:     count(0),
		AudioDurationMillis: count(0),
	}
	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"cache_read_tokens":0`)) {
		t.Fatalf("reported zero was omitted: %s", data)
	}
	if bytes.Contains(data, []byte(`"output"`)) {
		t.Fatalf("unreported output details were serialized: %s", data)
	}
	for _, field := range []string{
		`"generated_images":0`,
		`"input_characters":0`,
		`"audio_duration_millis":0`,
	} {
		if !bytes.Contains(data, []byte(field)) {
			t.Fatalf("reported zero %s was omitted: %s", field, data)
		}
	}
}

func TestUnifiedUsageValidatesGenerationDimensions(t *testing.T) {
	usage := Usage{
		InputTokens:         2,
		OutputTokens:        3,
		TotalTokens:         5,
		GeneratedImages:     count(1),
		InputCharacters:     count(12),
		AudioDurationMillis: count(750),
		Tools:               []ToolUsage{{Kind: ToolUsageWebSearch, Requests: 1}},
		Billing: &BillingUsage{
			Cost: &Money{Currency: "USD", Units: 1, Scale: 6},
		},
	}
	if err := usage.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for name, mutate := range map[string]func(*Usage){
		"generated images": func(u *Usage) { u.GeneratedImages = count(-1) },
		"input characters": func(u *Usage) { u.InputCharacters = count(-1) },
		"audio duration":   func(u *Usage) { u.AudioDurationMillis = count(-1) },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := usage
			mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatal("negative optional usage dimension accepted")
			}
		})
	}
}

func count(value int64) *int64 { return &value }
