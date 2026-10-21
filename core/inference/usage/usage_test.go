package usage

import (
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference/model"
)

// The accounting vocabulary is arithmetic and validation, and both have rules a
// reader cannot infer from the struct: which side of an Add wins, what stays
// nil, and which combinations are inconsistent. These pin them.

func int64Ptr(value int64) *int64 { return &value }

func TestUsageAdd(t *testing.T) {
	ref := model.ModelRef{ID: model.ModelID{Provider: "p", Name: "m"}}
	other := model.ModelRef{ID: model.ModelID{Provider: "p", Name: "other"}}

	t.Run("sums totals and keeps the first model", func(t *testing.T) {
		got := Usage{
			InputTokens: 3, OutputTokens: 4, TotalTokens: 7,
			Model: ref, LatencyMs: 5,
			Input: InputTokenUsage{CacheReadTokens: int64Ptr(1)},
		}.Add(Usage{
			InputTokens: 1, OutputTokens: 2, TotalTokens: 3,
			Model: other, LatencyMs: 6,
			Input: InputTokenUsage{CacheReadTokens: int64Ptr(2)},
		})
		if got.InputTokens != 4 || got.OutputTokens != 6 || got.TotalTokens != 10 {
			t.Fatalf("totals = %d/%d/%d, want 4/6/10",
				got.InputTokens, got.OutputTokens, got.TotalTokens)
		}
		if got.LatencyMs != 11 {
			t.Fatalf("latency = %d, want 11", got.LatencyMs)
		}
		if got.Model != ref {
			t.Fatalf("model = %v, want the left operand's", got.Model)
		}
		if got.Input.CacheReadTokens == nil || *got.Input.CacheReadTokens != 3 {
			t.Fatalf("cache read = %v, want 3", got.Input.CacheReadTokens)
		}
	})

	t.Run("takes the model from the right when the left has none", func(t *testing.T) {
		got := Usage{}.Add(Usage{Model: other})
		if got.Model != other {
			t.Fatalf("model = %v, want the right operand's", got.Model)
		}
	})

	t.Run("leaves an unreported counter nil", func(t *testing.T) {
		got := Usage{}.Add(Usage{})
		if got.Input.CacheReadTokens != nil || got.Input.UncachedTokens != nil {
			t.Fatal("a counter neither side reported must stay nil, not become zero")
		}
	})

	t.Run("does not carry the breakdowns it cannot sum", func(t *testing.T) {
		// Documented: modality splits, TTL-keyed cache writes, tool details and
		// billing have no provider-independent sum semantics.
		got := Usage{
			Auxiliary: []AuxiliaryTokenUsage{{Kind: AuxiliaryCitationTokens, Tokens: 2}},
			Tools:     []ToolUsage{{Kind: ToolUsageMCP, Requests: 1}},
			Billing:   &BillingUsage{Cost: &Money{Currency: "usd", Units: 1, Scale: 4}},
		}.Add(Usage{})
		if len(got.Auxiliary) != 0 || len(got.Tools) != 0 || got.Billing != nil {
			t.Fatalf("breakdowns carried over: auxiliary=%v tools=%v billing=%v",
				got.Auxiliary, got.Tools, got.Billing)
		}
	})
}

func TestMoneyAdd(t *testing.T) {
	sum, ok := Money{Currency: "usd", Units: 150, Scale: 4}.
		Add(Money{Currency: "usd", Units: 25, Scale: 4})
	if !ok {
		t.Fatal("same currency and scale must add")
	}
	if sum.Units != 175 || sum.Scale != 4 || sum.Currency != "usd" {
		t.Fatalf("sum = %+v, want 175 units at scale 4 usd", sum)
	}
	for name, other := range map[string]Money{
		"different currency": {Currency: "eur", Units: 1, Scale: 4},
		"different scale":    {Currency: "usd", Units: 1, Scale: 2},
	} {
		if _, ok := (Money{Currency: "usd", Units: 1, Scale: 4}).Add(other); ok {
			t.Fatalf("%s: Add reported ok; converting is a policy decision", name)
		}
	}
}

func TestUsageClone(t *testing.T) {
	original := Usage{
		InputTokens: 1,
		Input:       InputTokenUsage{CacheReadTokens: int64Ptr(7), ByModality: []ModalityTokenUsage{{Modality: ModalityText, Tokens: 1}}},
		Auxiliary:   []AuxiliaryTokenUsage{{Kind: AuxiliaryCitationTokens, Tokens: 2}},
		Billing:     &BillingUsage{Cost: &Money{Currency: "usd", Units: 1, Scale: 4}},
	}
	clone := original.Clone()
	*clone.Input.CacheReadTokens = 99
	clone.Input.ByModality[0].Tokens = 99
	clone.Auxiliary[0].Tokens = 99
	clone.Billing.Cost.Units = 99

	if *original.Input.CacheReadTokens != 7 || original.Input.ByModality[0].Tokens != 1 ||
		original.Auxiliary[0].Tokens != 2 || original.Billing.Cost.Units != 1 {
		t.Fatal("Clone shares storage with the original")
	}
}

func TestUsageValidate(t *testing.T) {
	valid := Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid usage rejected: %v", err)
	}

	tests := []struct {
		name  string
		usage Usage
		want  string
	}{
		{
			"negative total",
			Usage{InputTokens: -1, TotalTokens: -1},
			"non-negative",
		},
		{
			"total does not add up",
			Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 6},
			"total must equal input plus output",
		},
		{
			"negative latency",
			Usage{LatencyMs: -1},
			"latency must be non-negative",
		},
		{
			"negative optional counter",
			Usage{GeneratedImages: int64Ptr(-1)},
			"generated images",
		},
		{
			"model missing its name",
			Usage{Model: model.ModelRef{ID: model.ModelID{Provider: "p"}}},
			"usage model: model name is required",
		},
		{
			"billing without currency",
			Usage{Billing: &BillingUsage{Cost: &Money{Units: 1}}},
			"billing currency is required",
		},
		{
			"billing scale out of range",
			Usage{Billing: &BillingUsage{Cost: &Money{Currency: "usd", Scale: 19}}},
			"scale must not exceed 18",
		},
		{
			"negative billed units",
			Usage{Billing: &BillingUsage{Cost: &Money{Currency: "usd", Units: -1}}},
			"must be non-negative",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.usage.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", test.usage)
			}
			if test.want != "" && !contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want it to mention %q", err, test.want)
			}
		})
	}
}

// A modality breakdown is a subset of the total it breaks down. Under-reporting
// is what a driver does when a provider names only some modalities (OpenAI
// reports audio tokens, not text), so it must stay valid; over-reporting is a
// contradiction the totals rule cannot catch on its own.
func TestUsageValidateBoundsModalitiesByTheirTotal(t *testing.T) {
	tests := []struct {
		name  string
		usage Usage
		want  string
	}{
		{
			"input sum equals the prompt total",
			Usage{
				InputTokens: 10, OutputTokens: 1, TotalTokens: 11,
				Input: InputTokenUsage{ByModality: []ModalityTokenUsage{
					{Modality: ModalityText, Tokens: 8},
					{Modality: ModalityAudio, Tokens: 2},
				}},
			},
			"",
		},
		{
			"input sum below the prompt total stays valid",
			Usage{
				InputTokens: 10, OutputTokens: 1, TotalTokens: 11,
				Input: InputTokenUsage{ByModality: []ModalityTokenUsage{
					{Modality: ModalityAudio, Tokens: 2},
				}},
			},
			"",
		},
		{
			"input sum above the prompt total",
			Usage{
				InputTokens: 10, OutputTokens: 1, TotalTokens: 11,
				Input: InputTokenUsage{ByModality: []ModalityTokenUsage{
					{Modality: ModalityText, Tokens: 9},
					{Modality: ModalityAudio, Tokens: 2},
				}},
			},
			"input modality tokens total 11 exceeds input tokens 10",
		},
		{
			"output sum above the completion total",
			Usage{
				InputTokens: 1, OutputTokens: 3, TotalTokens: 4,
				Output: OutputTokenUsage{ByModality: []ModalityTokenUsage{
					{Modality: ModalityAudio, Tokens: 4},
				}},
			},
			"output modality tokens total 4 exceeds output tokens 3",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.usage.Validate()
			if test.want == "" {
				if err != nil {
					t.Fatalf("Validate rejected %+v: %v", test.usage, err)
				}
				return
			}
			if err == nil || !contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestUsageValidateSkipsTheZeroModel(t *testing.T) {
	// A reporter that only knows token counts leaves the envelope zero, and a
	// zero ModelRef means "not reported" rather than "invalid".
	if err := (Usage{InputTokens: 1, TotalTokens: 1}).Validate(); err != nil {
		t.Fatalf("a zero model must be accepted: %v", err)
	}
	// A partially filled one is a real error, and the model check wraps so the
	// inner refusal stays reachable.
	err := Usage{Model: model.ModelRef{ID: model.ModelID{Provider: "p"}}}.Validate()
	if err == nil {
		t.Fatal("a model with no name should be rejected")
	}
	if !contains(err.Error(), "usage model: ") {
		t.Fatalf("error = %v, want a wrapped usage model failure", err)
	}
	// The wrap is what keeps the inner refusal classifiable.
	if !errors.Is(err, err) {
		t.Fatalf("error = %v, want it to unwrap", err)
	}
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
