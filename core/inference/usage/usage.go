// Package usage is the token and cost accounting vocabulary: the per-call
// totals an engine reports, their breakdowns by modality, cache and tool, and
// the exact money a provider billed. It carries no runtime: a host implements
// usage reporting without depending on the inference packages that produce the
// numbers.
//
// # Roles
//
// [Usage] is one call's account: token totals, typed Work meters,
// per-modality and cache breakdowns, and [BillingUsage] for provider-reported
// billed units and [Money] cost. [Money]
// is units over 10^scale in a currency, so a provider's 10^-10 USD ticks
// survive without floating point. [Usage.Add] and [Money.Add] accumulate;
// [Money.Add] refuses a mismatched currency or scale, because converting
// between them is a policy decision this package does not make.
//
// # Invariants
//
// Every optional counter is a pointer, so "not reported" and "reported zero"
// stay distinguishable end to end. Totals are inclusive prompt counts: the
// cache buckets are disjoint subsets of Input, not additions to it, and a
// modality breakdown is a subset of its total as well — under-reporting means
// the rest is unattributed, over-reporting is a contradiction. Validate is the
// single consistency rule set (totals against their parts, modalities against
// their total) and runs where a response is validated, not on every read.
//
// # Boundary
//
// Producing the numbers is the drivers' and the runtime's job; deciding what
// they cost is the host's (a pricing catalog lives outside this package). This
// package only carries and adds them up.
package usage

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/internal/ptr"
)

// Usage contains normalized effective token totals plus optional
// provider-reported generation dimensions and breakdowns. Optional counters
// use pointers so an API can distinguish "not reported" from a reported zero.
//
// The call envelope is not here: the model lives on the response's Metadata
// and call latency on the run record, so Usage carries only the numbers a
// provider reported. Cost has exactly one home — Billing.Cost, a Money with
// explicit currency and scale — so there is never a second "cost" field to
// disagree with it.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	// Work carries non-token meters, keyed so a new modality is a new
	// WorkKind constant rather than another field.
	Work map[WorkKind]int64 `json:"work,omitempty"`

	// Input carries the cache split of InputTokens. Every bucket is a subset
	// of the inclusive prompt total, so the buckets sum to at most InputTokens.
	Input InputTokenUsage `json:"input,omitzero"`

	// Details carries the breakdowns that cannot be summed across calls:
	// modality splits, TTL-keyed cache writes, output token detail, auxiliary
	// counters and tool requests.
	Details Details `json:"details,omitzero"`

	// Billing carries provider-reported billed units and exact cost. It is a
	// price, not a meter, so AddMeter never touches it.
	Billing *Billing `json:"billing,omitempty"`
}

// InputTokenUsage splits Usage.InputTokens into disjoint cache buckets, each a
// subset of the inclusive input total. Drivers whose wire counters are
// exclusive normalize on the way in: Anthropic's input_tokens counts only the
// tokens that were neither read from nor written to the cache, so its
// canonical total is input_tokens + cache_read + cache_write and
// UncachedTokens carries the exclusive bucket unchanged.
//
// A nil counter means "zero, or not reported": drivers omit cache read / write
// when the provider reports zero, so a missing bucket is inferred from the
// total rather than from its siblings.
type InputTokenUsage struct {
	CacheReadTokens  *int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	UncachedTokens   *int64 `json:"uncached_tokens,omitempty"`
}

// OutputTokenUsage carries output-side detail whose sum semantics depend on
// the provider. It is a detail, not a meter: Usage.AddMeter does not sum it.
type OutputTokenUsage struct {
	ReasoningTokens          *int64              `json:"reasoning_tokens,omitempty"`
	ReasoningAccounting      ReasoningAccounting `json:"reasoning_accounting,omitempty"`
	AcceptedPredictionTokens *int64              `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens *int64              `json:"rejected_prediction_tokens,omitempty"`
}

// Details is the non-summable half of one call's account. It has no Add: a
// caller that wants to accumulate a breakdown does so explicitly.
type Details struct {
	InputModalities  map[Modality]int64           `json:"input_modalities,omitempty"`
	OutputModalities map[Modality]int64           `json:"output_modalities,omitempty"`
	CacheWrites      map[CacheTTL]int64           `json:"cache_writes,omitempty"`
	Auxiliary        map[AuxiliaryTokenKind]int64 `json:"auxiliary,omitempty"`
	Tools            map[ToolUsageKind]int64      `json:"tools,omitempty"`
	Output           OutputTokenUsage             `json:"output,omitzero"`
}

type ReasoningAccounting string

const (
	// ReasoningIncludedInOutput means ReasoningTokens is a subset of
	// Usage.OutputTokens.
	ReasoningIncludedInOutput ReasoningAccounting = "included_in_output"
	// ReasoningAdditional means the provider accounts for reasoning separately
	// from Usage.OutputTokens and the derived total.
	ReasoningAdditional ReasoningAccounting = "additional"
	// ReasoningAccountingUnknown preserves a reported count when inclusion
	// semantics are undocumented.
	ReasoningAccountingUnknown ReasoningAccounting = "unknown"
)

// WorkKind names a non-token meter. The unit is part of the name, so values of
// different kinds never mix even though they share an int64 representation.
type WorkKind string

const (
	// WorkImagesGenerated counts generated images.
	WorkImagesGenerated WorkKind = "images.generated"
	// WorkVideosGenerated counts generated videos.
	WorkVideosGenerated WorkKind = "videos.generated"
	// WorkInputCharacters counts characters sent to a speech surface.
	WorkInputCharacters WorkKind = "characters.input"
	// WorkAudioDurationMillis is generated audio duration in milliseconds.
	WorkAudioDurationMillis WorkKind = "audio.duration_ms"
	// WorkVideoDurationMillis is generated video duration in milliseconds.
	WorkVideoDurationMillis WorkKind = "video.duration_ms"
)

// Valid reports whether the kind is part of this contract's vocabulary.
func (k WorkKind) Valid() bool {
	switch k {
	case WorkImagesGenerated, WorkVideosGenerated, WorkInputCharacters,
		WorkAudioDurationMillis, WorkVideoDurationMillis:
		return true
	}
	return false
}

type Modality string

const (
	ModalityText  Modality = "text"
	ModalityImage Modality = "image"
	ModalityAudio Modality = "audio"
	ModalityVideo Modality = "video"
	ModalityFile  Modality = "file"
)

type CacheTTL string

const (
	CacheTTL5Minutes CacheTTL = "5m"
	CacheTTL1Hour    CacheTTL = "1h"
)

type AuxiliaryTokenKind string

const (
	AuxiliaryCitationTokens   AuxiliaryTokenKind = "citation"
	AuxiliaryToolPromptTokens AuxiliaryTokenKind = "tool_prompt"
)

type ToolUsageKind string

const (
	ToolUsageWebSearch     ToolUsageKind = "web_search"
	ToolUsageWebFetch      ToolUsageKind = "web_fetch"
	ToolUsageCodeExecution ToolUsageKind = "code_execution"
	ToolUsageMCP           ToolUsageKind = "mcp"
	ToolUsageXSearch       ToolUsageKind = "x_search"
	ToolUsageSearchQuery   ToolUsageKind = "search_query"
)

// Billing retains provider-reported billed units and exact cost without
// conflating them with raw model token counts.
type Billing struct {
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

// Add sums two Money values that share currency and scale, so hosts
// can accumulate cost under their own currency policy. It returns
// ok=false for mismatched currency or scale: converting between them
// is a policy decision (exchange rates, precision) this type does not
// make. Callers enforcing a single budget currency should normalize or
// reject on ok=false rather than drop the amount.
func (m Money) Add(other Money) (Money, bool) {
	if m.Currency != other.Currency || m.Scale != other.Scale {
		return Money{}, false
	}
	return Money{Currency: m.Currency, Scale: m.Scale, Units: m.Units + other.Units}, true
}

func (u Usage) Clone() Usage {
	u.Work = cloneMap(u.Work)
	u.Input = u.Input.Clone()
	u.Details = u.Details.Clone()
	if u.Billing != nil {
		billing := u.Billing.Clone()
		u.Billing = &billing
	}
	return u
}

func (u InputTokenUsage) Clone() InputTokenUsage {
	u.CacheReadTokens = ptr.Clone(u.CacheReadTokens)
	u.CacheWriteTokens = ptr.Clone(u.CacheWriteTokens)
	u.UncachedTokens = ptr.Clone(u.UncachedTokens)
	return u
}

func (u OutputTokenUsage) Clone() OutputTokenUsage {
	u.ReasoningTokens = ptr.Clone(u.ReasoningTokens)
	u.AcceptedPredictionTokens = ptr.Clone(u.AcceptedPredictionTokens)
	u.RejectedPredictionTokens = ptr.Clone(u.RejectedPredictionTokens)
	return u
}

func (d Details) Clone() Details {
	d.InputModalities = cloneMap(d.InputModalities)
	d.OutputModalities = cloneMap(d.OutputModalities)
	d.CacheWrites = cloneMap(d.CacheWrites)
	d.Auxiliary = cloneMap(d.Auxiliary)
	d.Tools = cloneMap(d.Tools)
	d.Output = d.Output.Clone()
	return d
}

func (b Billing) Clone() Billing {
	b.InputTokens = ptr.Clone(b.InputTokens)
	b.OutputTokens = ptr.Clone(b.OutputTokens)
	b.Cost = ptr.Clone(b.Cost)
	return b
}

func cloneMap[K comparable, V any](in map[K]V) map[K]V {
	if in == nil {
		return nil
	}
	out := make(map[K]V, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// AddMeter sums the meters a budget accumulator sums: token totals, the
// input-side cache counters, and the typed Work meters. It deliberately does
// not touch Details or Billing: a breakdown and a price have no
// provider-independent sum semantics.
func (u Usage) AddMeter(other Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + other.InputTokens,
		OutputTokens: u.OutputTokens + other.OutputTokens,
		Work:         mergeWork(u.Work, other.Work),
		Input: InputTokenUsage{
			CacheReadTokens:  addPointers(u.Input.CacheReadTokens, other.Input.CacheReadTokens),
			CacheWriteTokens: addPointers(u.Input.CacheWriteTokens, other.Input.CacheWriteTokens),
			UncachedTokens:   addPointers(u.Input.UncachedTokens, other.Input.UncachedTokens),
		},
	}
}

// mergeWork sums two Work maps; two empty maps stay nil so "not reported" does
// not become an empty reported map.
func mergeWork(a, b map[WorkKind]int64) map[WorkKind]int64 {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[WorkKind]int64, len(a)+len(b))
	for kind, value := range a {
		out[kind] += value
	}
	for kind, value := range b {
		out[kind] += value
	}
	return out
}

// addPointers sums two optional counters; both nil stays nil so "not
// reported" does not become a reported zero.
func addPointers(a, b *int64) *int64 {
	if a == nil && b == nil {
		return nil
	}
	var sum int64
	if a != nil {
		sum += *a
	}
	if b != nil {
		sum += *b
	}
	return &sum
}

// TotalTokens is the inclusive prompt plus generated total. It is derived, so
// it can never disagree with its parts.
func (u Usage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// WorkValue returns one meter, or zero when it was not reported.
func (u Usage) WorkValue(kind WorkKind) int64 { return u.Work[kind] }

// AddWork accumulates one meter.
func (u *Usage) AddWork(kind WorkKind, value int64) {
	if u.Work == nil {
		u.Work = make(map[WorkKind]int64, 1)
	}
	u.Work[kind] += value
}

func (u Usage) Validate() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 {
		return fmt.Errorf("usage token totals must be non-negative")
	}
	for kind, value := range u.Work {
		if !kind.Valid() {
			return fmt.Errorf("unknown work kind %q", kind)
		}
		if value < 0 {
			return fmt.Errorf("%s work must be non-negative", kind)
		}
	}
	if err := u.Input.ValidateFor(u.InputTokens); err != nil {
		return fmt.Errorf("input token usage: %w", err)
	}
	if err := u.Details.ValidateFor(
		u.InputTokens, u.OutputTokens, u.Input.CacheWriteTokens,
	); err != nil {
		return fmt.Errorf("usage details: %w", err)
	}
	if u.Billing != nil {
		if err := u.Billing.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ValidateFor checks the input-side cache counters against the prompt total.
func (u InputTokenUsage) ValidateFor(inputTokens int64) error {
	if err := validateOptionalCount("cache read", u.CacheReadTokens); err != nil {
		return err
	}
	if err := validateOptionalCount("cache write", u.CacheWriteTokens); err != nil {
		return err
	}
	return validateOptionalCount("uncached", u.UncachedTokens)
}

// ValidateFor checks the breakdowns against the totals they break down. A
// breakdown may under-report — a driver attributes only the modalities a
// provider names — but never exceed its total.
func (d Details) ValidateFor(
	inputTokens, outputTokens int64, cacheWriteTokens *int64,
) error {
	if err := validateModalityMap("input", d.InputModalities, inputTokens); err != nil {
		return err
	}
	if err := validateModalityMap("output", d.OutputModalities, outputTokens); err != nil {
		return err
	}
	if err := validateCacheWrites(d.CacheWrites, cacheWriteTokens); err != nil {
		return err
	}
	if err := validateAuxiliary(d.Auxiliary); err != nil {
		return err
	}
	if err := validateTools(d.Tools); err != nil {
		return err
	}
	return d.Output.ValidateFor(outputTokens)
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
	return nil
}

func validateModalityMap(kind string, usages map[Modality]int64, total int64) error {
	var sum int64
	for modality, tokens := range usages {
		switch modality {
		case ModalityText, ModalityImage, ModalityAudio, ModalityVideo, ModalityFile:
		default:
			return fmt.Errorf("unknown modality %q", modality)
		}
		if tokens < 0 {
			return fmt.Errorf("%s modality tokens must be non-negative", modality)
		}
		sum += tokens
	}
	if total > 0 && sum > total {
		return fmt.Errorf(
			"%s modality tokens total %d exceeds %s tokens %d", kind, sum, kind, total)
	}
	return nil
}

func validateCacheWrites(writes map[CacheTTL]int64, total *int64) error {
	var sum int64
	for ttl, tokens := range writes {
		switch ttl {
		case CacheTTL5Minutes, CacheTTL1Hour:
		default:
			return fmt.Errorf("unknown cache TTL %q", ttl)
		}
		if tokens < 0 {
			return fmt.Errorf("cache write tokens must be non-negative")
		}
		sum += tokens
	}
	if total != nil && len(writes) > 0 && sum != *total {
		return fmt.Errorf("cache write breakdown does not match cache write tokens")
	}
	return nil
}

func validateAuxiliary(usages map[AuxiliaryTokenKind]int64) error {
	for kind, tokens := range usages {
		switch kind {
		case AuxiliaryCitationTokens, AuxiliaryToolPromptTokens:
		default:
			return fmt.Errorf("unknown auxiliary token kind %q", kind)
		}
		if tokens < 0 {
			return fmt.Errorf("%s tokens must be non-negative", kind)
		}
	}
	return nil
}

func validateTools(usages map[ToolUsageKind]int64) error {
	for kind, requests := range usages {
		switch kind {
		case ToolUsageWebSearch, ToolUsageWebFetch, ToolUsageCodeExecution,
			ToolUsageMCP, ToolUsageXSearch, ToolUsageSearchQuery:
		default:
			return fmt.Errorf("unknown tool usage kind %q", kind)
		}
		if requests < 0 {
			return fmt.Errorf("%s requests must be non-negative", kind)
		}
	}
	return nil
}

func (b Billing) Validate() error {
	if err := validateOptionalCount("billed input", b.InputTokens); err != nil {
		return err
	}
	if err := validateOptionalCount("billed output", b.OutputTokens); err != nil {
		return err
	}
	if b.Cost != nil {
		if err := b.Cost.Validate(); err != nil {
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
