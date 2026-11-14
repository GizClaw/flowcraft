package usage

import (
	"testing"

	"github.com/goccy/go-json"
)

// The accounting vocabulary is arithmetic and validation, and both have rules a
// reader cannot infer from the struct: which side of AddMeter wins, what stays
// nil, which breakdowns are not summed, and which combinations are inconsistent.

func int64Ptr(value int64) *int64 { return &value }

func TestUsageAddMeter(t *testing.T) {
	t.Run("sums tokens, cache counters and work", func(t *testing.T) {
		got := Usage{
			InputTokens: 3, OutputTokens: 4,
			Input: InputTokenUsage{CacheReadTokens: int64Ptr(1)},
			Work:  map[WorkKind]int64{WorkImagesGenerated: 2},
		}.AddMeter(Usage{
			InputTokens: 1, OutputTokens: 2,
			Input: InputTokenUsage{CacheReadTokens: int64Ptr(2)},
			Work:  map[WorkKind]int64{WorkVideosGenerated: 1},
		})
		if got.InputTokens != 4 || got.OutputTokens != 6 || got.TotalTokens() != 10 {
			t.Fatalf("totals = %d/%d/%d, want 4/6/10",
				got.InputTokens, got.OutputTokens, got.TotalTokens())
		}
		if got.Input.CacheReadTokens == nil || *got.Input.CacheReadTokens != 3 {
			t.Fatalf("cache read = %v, want 3", got.Input.CacheReadTokens)
		}
		if got.WorkValue(WorkImagesGenerated) != 2 || got.WorkValue(WorkVideosGenerated) != 1 {
			t.Fatalf("work = %v, want images=2 videos=1", got.Work)
		}
	})

	t.Run("leaves an unreported counter nil", func(t *testing.T) {
		got := Usage{}.AddMeter(Usage{})
		if got.Input.CacheReadTokens != nil || got.Input.UncachedTokens != nil {
			t.Fatal("a counter neither side reported must stay nil, not become zero")
		}
	})

	t.Run("does not carry details or billing", func(t *testing.T) {
		got := Usage{
			Details: Details{
				InputModalities: map[Modality]int64{ModalityText: 1},
				Auxiliary:       map[AuxiliaryTokenKind]int64{AuxiliaryCitationTokens: 2},
				Tools:           map[ToolUsageKind]int64{ToolUsageMCP: 1},
			},
			Billing: &Billing{Cost: &Money{Currency: "usd", Units: 1, Scale: 4}},
		}.AddMeter(Usage{})
		if len(got.Details.InputModalities) != 0 ||
			len(got.Details.Auxiliary) != 0 ||
			len(got.Details.Tools) != 0 ||
			got.Billing != nil {
			t.Fatalf("details carried over: %+v billing=%v", got.Details, got.Billing)
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
		Work:        map[WorkKind]int64{WorkImagesGenerated: 1},
		Input:       InputTokenUsage{CacheReadTokens: int64Ptr(7)},
		Details: Details{
			InputModalities: map[Modality]int64{ModalityText: 1},
			CacheWrites:     map[CacheTTL]int64{CacheTTL5Minutes: 2},
			Auxiliary:       map[AuxiliaryTokenKind]int64{AuxiliaryCitationTokens: 2},
			Output:          OutputTokenUsage{ReasoningTokens: int64Ptr(3)},
		},
		Billing: &Billing{Cost: &Money{Currency: "usd", Units: 1, Scale: 4}},
	}
	clone := original.Clone()
	*clone.Input.CacheReadTokens = 99
	clone.Work[WorkImagesGenerated] = 99
	clone.Details.InputModalities[ModalityText] = 99
	clone.Details.CacheWrites[CacheTTL5Minutes] = 99
	clone.Details.Auxiliary[AuxiliaryCitationTokens] = 99
	*clone.Details.Output.ReasoningTokens = 99
	clone.Billing.Cost.Units = 99

	if *original.Input.CacheReadTokens != 7 ||
		original.WorkValue(WorkImagesGenerated) != 1 ||
		original.Details.InputModalities[ModalityText] != 1 ||
		original.Details.CacheWrites[CacheTTL5Minutes] != 2 ||
		original.Details.Auxiliary[AuxiliaryCitationTokens] != 2 ||
		*original.Details.Output.ReasoningTokens != 3 ||
		original.Billing.Cost.Units != 1 {
		t.Fatal("Clone shares storage with the original")
	}
}

func TestUsageValidate(t *testing.T) {
	valid := Usage{InputTokens: 2, OutputTokens: 3}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid usage rejected: %v", err)
	}

	tests := []struct {
		name  string
		usage Usage
		want  string
	}{
		{
			"negative prompt tokens",
			Usage{InputTokens: -1},
			"non-negative",
		},
		{
			"negative output tokens",
			Usage{OutputTokens: -1},
			"non-negative",
		},
		{
			"unknown work kind",
			Usage{Work: map[WorkKind]int64{WorkKind("typo"): 1}},
			"unknown work kind",
		},
		{
			"negative work",
			Usage{Work: map[WorkKind]int64{WorkImagesGenerated: -1}},
			"must be non-negative",
		},
		{
			"billing without currency",
			Usage{Billing: &Billing{Cost: &Money{Units: 1}}},
			"billing currency is required",
		},
		{
			"billing scale out of range",
			Usage{Billing: &Billing{Cost: &Money{Currency: "usd", Scale: 19}}},
			"scale must not exceed 18",
		},
		{
			"negative billed units",
			Usage{Billing: &Billing{Cost: &Money{Currency: "usd", Units: -1}}},
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
// is what a driver does when a provider names only some modalities; a total
// nobody reported is not a total to be exceeded.
func TestUsageValidateBoundsModalitiesByTheirTotal(t *testing.T) {
	tests := []struct {
		name  string
		usage Usage
		want  string
	}{
		{
			"input sum equals the prompt total",
			Usage{
				InputTokens: 10, OutputTokens: 1,
				Details: Details{InputModalities: map[Modality]int64{
					ModalityText:  8,
					ModalityAudio: 2,
				}},
			},
			"",
		},
		{
			"input sum below the prompt total stays valid",
			Usage{
				InputTokens: 10, OutputTokens: 1,
				Details: Details{InputModalities: map[Modality]int64{
					ModalityAudio: 2,
				}},
			},
			"",
		},
		{
			"input breakdown without a reported total",
			Usage{
				Details: Details{InputModalities: map[Modality]int64{
					ModalityAudio: 100,
				}},
			},
			"",
		},
		{
			"input sum above the prompt total",
			Usage{
				InputTokens: 10, OutputTokens: 1,
				Details: Details{InputModalities: map[Modality]int64{
					ModalityText:  9,
					ModalityAudio: 2,
				}},
			},
			"input modality tokens total 11 exceeds input tokens 10",
		},
		{
			"output sum above the completion total",
			Usage{
				InputTokens: 1, OutputTokens: 3,
				Details: Details{OutputModalities: map[Modality]int64{
					ModalityAudio: 4,
				}},
			},
			"output modality tokens total 4 exceeds output tokens 3",
		},
		{
			"cache write breakdown mismatch",
			Usage{
				Input: InputTokenUsage{CacheWriteTokens: int64Ptr(5)},
				Details: Details{CacheWrites: map[CacheTTL]int64{
					CacheTTL5Minutes: 2,
				}},
			},
			"cache write breakdown does not match cache write tokens",
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

// Work is the extension point: a new modality adds a WorkKind constant, not a
// Usage field, and AddMeter merges it like any other meter.
func TestUsageWork(t *testing.T) {
	var total Usage
	total.AddWork(WorkImagesGenerated, 2)
	total.AddWork(WorkImagesGenerated, 3)
	if got := total.WorkValue(WorkImagesGenerated); got != 5 {
		t.Fatalf("images = %d, want 5", got)
	}

	sum := total.AddMeter(Usage{Work: map[WorkKind]int64{
		WorkVideosGenerated: 1,
	}})
	if got := sum.WorkValue(WorkImagesGenerated); got != 5 {
		t.Fatalf("summed images = %d, want 5", got)
	}
	if got := sum.WorkValue(WorkVideosGenerated); got != 1 {
		t.Fatalf("summed videos = %d, want 1", got)
	}
	if got := (Usage{}).AddMeter(Usage{}).Work; got != nil {
		t.Fatalf("empty work = %v, want nil", got)
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

// The wire shape is the contract a trace exporter and a billing job read. Work
// is keyed, details and billing are nested, and the legacy per-modality fields
// are gone.
func TestUsageJSONShape(t *testing.T) {
	raw, err := json.Marshal(Usage{
		InputTokens:  3,
		OutputTokens: 4,
		Work:         map[WorkKind]int64{WorkImagesGenerated: 1},
		Details: Details{
			InputModalities: map[Modality]int64{ModalityText: 2},
		},
		Billing: &Billing{Cost: &Money{Currency: "usd", Units: 1, Scale: 4}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"input_tokens":3,"output_tokens":4,"work":{"images.generated":1},` +
		`"details":{"input_modalities":{"text":2}},` +
		`"billing":{"cost":{"currency":"usd","units":1,"scale":4}}}`
	if string(raw) != want {
		t.Fatalf("JSON = %s\nwant %s", raw, want)
	}
}
