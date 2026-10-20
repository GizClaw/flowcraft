package inference

import "github.com/GizClaw/flowcraft/core/inference/usage"

// This file keeps the core/inference spellings of the token and cost
// vocabulary working after it moved to core/inference/usage. They are
// compatibility shims for existing callers — drivers, hosts, script bridges —
// so the move itself changed no import path and no behavior.
//
// New code should import core/inference/usage directly: that package carries
// the accounting vocabulary and nothing else, while this one carries the whole
// execution contract.

// Usage is one call's token and cost account.
type Usage = usage.Usage

// InputTokenUsage is the prompt-side breakdown.
type InputTokenUsage = usage.InputTokenUsage

// OutputTokenUsage is the completion-side breakdown.
type OutputTokenUsage = usage.OutputTokenUsage

// ReasoningAccounting says whether reasoning tokens are part of the output
// count.
type ReasoningAccounting = usage.ReasoningAccounting

// Modality names a content kind a token count is broken down by.
type Modality = usage.Modality

// ModalityTokenUsage is one modality's share of a token count.
type ModalityTokenUsage = usage.ModalityTokenUsage

// CacheTTL is a provider's prompt-cache retention class.
type CacheTTL = usage.CacheTTL

// CacheWriteUsage is one cache-write bucket.
type CacheWriteUsage = usage.CacheWriteUsage

// AuxiliaryTokenKind names a non-modality token bucket (citations, tool
// prompts).
type AuxiliaryTokenKind = usage.AuxiliaryTokenKind

// AuxiliaryTokenUsage is one auxiliary bucket's count.
type AuxiliaryTokenUsage = usage.AuxiliaryTokenUsage

// ToolUsageKind names the tool category a request count belongs to.
type ToolUsageKind = usage.ToolUsageKind

// ToolUsage is one tool category's request count.
type ToolUsage = usage.ToolUsage

// BillingUsage is what a provider billed, separately from raw token counts.
type BillingUsage = usage.BillingUsage

// Money is units over 10^scale in a currency.
type Money = usage.Money

// The classifier values below travel with the types above.
const (
	ReasoningIncludedInOutput  usage.ReasoningAccounting = usage.ReasoningIncludedInOutput
	ReasoningAdditional        usage.ReasoningAccounting = usage.ReasoningAdditional
	ReasoningAccountingUnknown usage.ReasoningAccounting = usage.ReasoningAccountingUnknown

	ModalityText  usage.Modality = usage.ModalityText
	ModalityImage usage.Modality = usage.ModalityImage
	ModalityAudio usage.Modality = usage.ModalityAudio
	ModalityVideo usage.Modality = usage.ModalityVideo
	ModalityFile  usage.Modality = usage.ModalityFile

	CacheTTL5Minutes usage.CacheTTL = usage.CacheTTL5Minutes
	CacheTTL1Hour    usage.CacheTTL = usage.CacheTTL1Hour

	AuxiliaryCitationTokens   usage.AuxiliaryTokenKind = usage.AuxiliaryCitationTokens
	AuxiliaryToolPromptTokens usage.AuxiliaryTokenKind = usage.AuxiliaryToolPromptTokens

	ToolUsageWebSearch     usage.ToolUsageKind = usage.ToolUsageWebSearch
	ToolUsageWebFetch      usage.ToolUsageKind = usage.ToolUsageWebFetch
	ToolUsageCodeExecution usage.ToolUsageKind = usage.ToolUsageCodeExecution
	ToolUsageMCP           usage.ToolUsageKind = usage.ToolUsageMCP
	ToolUsageXSearch       usage.ToolUsageKind = usage.ToolUsageXSearch
	ToolUsageSearchQuery   usage.ToolUsageKind = usage.ToolUsageSearchQuery
)
