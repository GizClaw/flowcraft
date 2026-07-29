package inference

import "fmt"

// Usage contains normalized effective token totals plus optional
// provider-reported generation dimensions and breakdowns. Optional counters
// use pointers so an API can distinguish "not reported" from a reported zero.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`

	GeneratedImages     *int64 `json:"generated_images,omitempty"`
	GeneratedVideos     *int64 `json:"generated_videos,omitempty"`
	InputCharacters     *int64 `json:"input_characters,omitempty"`
	AudioDurationMillis *int64 `json:"audio_duration_millis,omitempty"`

	Input  InputTokenUsage  `json:"input,omitzero"`
	Output OutputTokenUsage `json:"output,omitzero"`

	Auxiliary []AuxiliaryTokenUsage `json:"auxiliary,omitempty"`
	Tools     []ToolUsage           `json:"tools,omitempty"`
	Billing   *BillingUsage         `json:"billing,omitempty"`
}

type InputTokenUsage struct {
	CacheReadTokens  *int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	UncachedTokens   *int64 `json:"uncached_tokens,omitempty"`

	ByModality  []ModalityTokenUsage `json:"by_modality,omitempty"`
	CacheWrites []CacheWriteUsage    `json:"cache_writes,omitempty"`
}

type OutputTokenUsage struct {
	ReasoningTokens          *int64              `json:"reasoning_tokens,omitempty"`
	ReasoningAccounting      ReasoningAccounting `json:"reasoning_accounting,omitempty"`
	AcceptedPredictionTokens *int64              `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens *int64              `json:"rejected_prediction_tokens,omitempty"`

	ByModality []ModalityTokenUsage `json:"by_modality,omitempty"`
}

type ReasoningAccounting string

const (
	// ReasoningIncludedInOutput means ReasoningTokens is a subset of
	// Usage.OutputTokens.
	ReasoningIncludedInOutput ReasoningAccounting = "included_in_output"
	// ReasoningAdditional means the provider accounts for reasoning separately
	// from Usage.OutputTokens and TotalTokens.
	ReasoningAdditional ReasoningAccounting = "additional"
	// ReasoningAccountingUnknown preserves a reported count when inclusion
	// semantics are undocumented.
	ReasoningAccountingUnknown ReasoningAccounting = "unknown"
)

type Modality string

const (
	ModalityText  Modality = "text"
	ModalityImage Modality = "image"
	ModalityAudio Modality = "audio"
	ModalityVideo Modality = "video"
	ModalityFile  Modality = "file"
)

type ModalityTokenUsage struct {
	Modality Modality `json:"modality"`
	Tokens   int64    `json:"tokens"`
}

type CacheTTL string

const (
	CacheTTL5Minutes CacheTTL = "5m"
	CacheTTL1Hour    CacheTTL = "1h"
)

type CacheWriteUsage struct {
	TTL    CacheTTL `json:"ttl"`
	Tokens int64    `json:"tokens"`
}

type AuxiliaryTokenKind string

const (
	AuxiliaryCitationTokens   AuxiliaryTokenKind = "citation"
	AuxiliaryToolPromptTokens AuxiliaryTokenKind = "tool_prompt"
)

// AuxiliaryTokenUsage records token counters that a provider accounts for
// outside the normalized input/output totals.
type AuxiliaryTokenUsage struct {
	Kind   AuxiliaryTokenKind `json:"kind"`
	Tokens int64              `json:"tokens"`
}

type ToolUsageKind string

const (
	ToolUsageWebSearch     ToolUsageKind = "web_search"
	ToolUsageWebFetch      ToolUsageKind = "web_fetch"
	ToolUsageCodeExecution ToolUsageKind = "code_execution"
	ToolUsageMCP           ToolUsageKind = "mcp"
	ToolUsageXSearch       ToolUsageKind = "x_search"
	ToolUsageSearchQuery   ToolUsageKind = "search_query"
)

type ToolUsage struct {
	Kind     ToolUsageKind `json:"kind"`
	Requests int64         `json:"requests"`
}

// BillingUsage retains provider-reported billed units and exact cost without
// conflating them with raw model token counts.
type BillingUsage struct {
	InputTokens  *int64 `json:"input_tokens,omitempty"`
	OutputTokens *int64 `json:"output_tokens,omitempty"`
	Cost         *Money `json:"cost,omitempty"`
}

// Money represents Units / 10^Scale in Currency. This preserves APIs such as
// xAI's 10^-10 USD ticks without floating-point rounding.
type Money struct {
	Currency string `json:"currency"`
	Units    int64  `json:"units"`
	Scale    uint8  `json:"scale"`
}

func (u Usage) Clone() Usage {
	u.GeneratedImages = clonePointer(u.GeneratedImages)
	u.GeneratedVideos = clonePointer(u.GeneratedVideos)
	u.InputCharacters = clonePointer(u.InputCharacters)
	u.AudioDurationMillis = clonePointer(u.AudioDurationMillis)
	u.Input.CacheReadTokens = clonePointer(u.Input.CacheReadTokens)
	u.Input.CacheWriteTokens = clonePointer(u.Input.CacheWriteTokens)
	u.Input.UncachedTokens = clonePointer(u.Input.UncachedTokens)
	u.Input.ByModality = append([]ModalityTokenUsage(nil), u.Input.ByModality...)
	u.Input.CacheWrites = append([]CacheWriteUsage(nil), u.Input.CacheWrites...)
	u.Output.ReasoningTokens = clonePointer(u.Output.ReasoningTokens)
	u.Output.AcceptedPredictionTokens = clonePointer(
		u.Output.AcceptedPredictionTokens,
	)
	u.Output.RejectedPredictionTokens = clonePointer(
		u.Output.RejectedPredictionTokens,
	)
	u.Output.ByModality = append([]ModalityTokenUsage(nil), u.Output.ByModality...)
	u.Auxiliary = append([]AuxiliaryTokenUsage(nil), u.Auxiliary...)
	u.Tools = append([]ToolUsage(nil), u.Tools...)
	if u.Billing != nil {
		billing := *u.Billing
		billing.InputTokens = clonePointer(billing.InputTokens)
		billing.OutputTokens = clonePointer(billing.OutputTokens)
		billing.Cost = clonePointer(billing.Cost)
		u.Billing = &billing
	}
	return u
}

func (u Usage) Validate() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.TotalTokens < 0 {
		return fmt.Errorf("usage token totals must be non-negative")
	}
	if u.TotalTokens != u.InputTokens+u.OutputTokens {
		return fmt.Errorf("usage total must equal input plus output tokens")
	}
	for name, value := range map[string]*int64{
		"generated images":            u.GeneratedImages,
		"generated videos":            u.GeneratedVideos,
		"input characters":            u.InputCharacters,
		"audio duration milliseconds": u.AudioDurationMillis,
	} {
		if err := validateOptionalCount(name, value); err != nil {
			return err
		}
	}
	if err := u.Input.Validate(); err != nil {
		return fmt.Errorf("input token usage: %w", err)
	}
	if err := u.Output.ValidateFor(u.OutputTokens); err != nil {
		return fmt.Errorf("output token usage: %w", err)
	}
	if err := validateAuxiliaryUsage(u.Auxiliary); err != nil {
		return err
	}
	if err := validateToolUsage(u.Tools); err != nil {
		return err
	}
	if u.Billing != nil {
		if err := u.Billing.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (u InputTokenUsage) Validate() error {
	if err := validateOptionalCount("cache read", u.CacheReadTokens); err != nil {
		return err
	}
	if err := validateOptionalCount("cache write", u.CacheWriteTokens); err != nil {
		return err
	}
	if err := validateOptionalCount("uncached", u.UncachedTokens); err != nil {
		return err
	}
	if err := validateModalities(u.ByModality); err != nil {
		return err
	}
	var cacheWriteTotal int64
	seenTTLs := make(map[CacheTTL]struct{}, len(u.CacheWrites))
	for _, write := range u.CacheWrites {
		switch write.TTL {
		case CacheTTL5Minutes, CacheTTL1Hour:
		default:
			return fmt.Errorf("unknown cache TTL %q", write.TTL)
		}
		if write.Tokens < 0 {
			return fmt.Errorf("cache write tokens must be non-negative")
		}
		if _, ok := seenTTLs[write.TTL]; ok {
			return fmt.Errorf("duplicate cache TTL %q", write.TTL)
		}
		seenTTLs[write.TTL] = struct{}{}
		cacheWriteTotal += write.Tokens
	}
	if u.CacheWriteTokens != nil && len(u.CacheWrites) > 0 &&
		cacheWriteTotal != *u.CacheWriteTokens {
		return fmt.Errorf("cache write breakdown does not match cache write tokens")
	}
	return nil
}

func (u OutputTokenUsage) ValidateFor(outputTokens int64) error {
	if err := validateOptionalCount("reasoning", u.ReasoningTokens); err != nil {
		return err
	}
	if err := validateOptionalCount("accepted prediction", u.AcceptedPredictionTokens); err != nil {
		return err
	}
	if err := validateOptionalCount("rejected prediction", u.RejectedPredictionTokens); err != nil {
		return err
	}
	if u.ReasoningTokens == nil {
		if u.ReasoningAccounting != "" {
			return fmt.Errorf("reasoning accounting requires reasoning tokens")
		}
	} else {
		switch u.ReasoningAccounting {
		case ReasoningIncludedInOutput:
			if *u.ReasoningTokens > outputTokens {
				return fmt.Errorf("included reasoning tokens exceed output tokens")
			}
		case ReasoningAdditional, ReasoningAccountingUnknown:
		default:
			return fmt.Errorf("reasoning accounting is required")
		}
	}
	return validateModalities(u.ByModality)
}

func validateModalities(usages []ModalityTokenUsage) error {
	seen := make(map[Modality]struct{}, len(usages))
	for _, usage := range usages {
		switch usage.Modality {
		case ModalityText, ModalityImage, ModalityAudio, ModalityVideo, ModalityFile:
		default:
			return fmt.Errorf("unknown modality %q", usage.Modality)
		}
		if usage.Tokens < 0 {
			return fmt.Errorf("%s modality tokens must be non-negative", usage.Modality)
		}
		if _, ok := seen[usage.Modality]; ok {
			return fmt.Errorf("duplicate modality %q", usage.Modality)
		}
		seen[usage.Modality] = struct{}{}
	}
	return nil
}

func validateAuxiliaryUsage(usages []AuxiliaryTokenUsage) error {
	seen := make(map[AuxiliaryTokenKind]struct{}, len(usages))
	for _, usage := range usages {
		switch usage.Kind {
		case AuxiliaryCitationTokens, AuxiliaryToolPromptTokens:
		default:
			return fmt.Errorf("unknown auxiliary token kind %q", usage.Kind)
		}
		if usage.Tokens < 0 {
			return fmt.Errorf("%s tokens must be non-negative", usage.Kind)
		}
		if _, ok := seen[usage.Kind]; ok {
			return fmt.Errorf("duplicate auxiliary token kind %q", usage.Kind)
		}
		seen[usage.Kind] = struct{}{}
	}
	return nil
}

func validateToolUsage(usages []ToolUsage) error {
	seen := make(map[ToolUsageKind]struct{}, len(usages))
	for _, usage := range usages {
		switch usage.Kind {
		case ToolUsageWebSearch, ToolUsageWebFetch, ToolUsageCodeExecution,
			ToolUsageMCP, ToolUsageXSearch, ToolUsageSearchQuery:
		default:
			return fmt.Errorf("unknown tool usage kind %q", usage.Kind)
		}
		if usage.Requests < 0 {
			return fmt.Errorf("%s requests must be non-negative", usage.Kind)
		}
		if _, ok := seen[usage.Kind]; ok {
			return fmt.Errorf("duplicate tool usage kind %q", usage.Kind)
		}
		seen[usage.Kind] = struct{}{}
	}
	return nil
}

func (u BillingUsage) Validate() error {
	if err := validateOptionalCount("billed input", u.InputTokens); err != nil {
		return err
	}
	if err := validateOptionalCount("billed output", u.OutputTokens); err != nil {
		return err
	}
	if u.Cost != nil {
		if err := u.Cost.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (m Money) Validate() error {
	if m.Currency == "" {
		return fmt.Errorf("billing currency is required")
	}
	if m.Units < 0 {
		return fmt.Errorf("billing cost must be non-negative")
	}
	if m.Scale > 18 {
		return fmt.Errorf("billing cost scale must not exceed 18")
	}
	return nil
}

func validateOptionalCount(name string, value *int64) error {
	if value != nil && *value < 0 {
		return fmt.Errorf("%s must be non-negative", name)
	}
	return nil
}
