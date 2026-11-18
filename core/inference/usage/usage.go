// Package usage forwards to core/inference/outcome.
//
// The token and cost vocabulary moved there, to sit beside the identity of the
// call it describes: one call's outcome is its [Metadata] and its account, and
// every result type in core/inference carries both as adjacent fields.
//
// # Why a shim and not a deletion
//
// The driver module is compiled twice — once against this checkout, once against
// the core version its go.mod pins — so a consumer cannot move in the same
// commit that renames the package. This forwarding layer is what lets the rename
// land first. It goes away with the release that moves the driver; nothing new
// belongs here.
package usage

import "github.com/GizClaw/flowcraft/core/inference/outcome"

// The accounting types, unchanged: an alias makes them the same type, so a value
// produced by core/inference/outcome is assignable here and back.
type (
	AuxiliaryTokenKind  = outcome.AuxiliaryTokenKind
	Billing             = outcome.Billing
	CacheTTL            = outcome.CacheTTL
	Details             = outcome.Details
	InputTokenUsage     = outcome.InputTokenUsage
	Modality            = outcome.Modality
	Money               = outcome.Money
	OutputTokenUsage    = outcome.OutputTokenUsage
	ReasoningAccounting = outcome.ReasoningAccounting
	ToolUsageKind       = outcome.ToolUsageKind
	Usage               = outcome.Usage
	WorkKind            = outcome.WorkKind
)

// The enumerated values, forwarded so a spelling that reads `usage.X` keeps
// compiling and keeps its type.
const (
	AuxiliaryCitationTokens    = outcome.AuxiliaryCitationTokens
	AuxiliaryToolPromptTokens  = outcome.AuxiliaryToolPromptTokens
	CacheTTL1Hour              = outcome.CacheTTL1Hour
	CacheTTL5Minutes           = outcome.CacheTTL5Minutes
	ModalityAudio              = outcome.ModalityAudio
	ModalityFile               = outcome.ModalityFile
	ModalityImage              = outcome.ModalityImage
	ModalityText               = outcome.ModalityText
	ModalityVideo              = outcome.ModalityVideo
	ReasoningAccountingUnknown = outcome.ReasoningAccountingUnknown
	ReasoningAdditional        = outcome.ReasoningAdditional
	ReasoningIncludedInOutput  = outcome.ReasoningIncludedInOutput
	ToolUsageCodeExecution     = outcome.ToolUsageCodeExecution
	ToolUsageMCP               = outcome.ToolUsageMCP
	ToolUsageSearchQuery       = outcome.ToolUsageSearchQuery
	ToolUsageWebFetch          = outcome.ToolUsageWebFetch
	ToolUsageWebSearch         = outcome.ToolUsageWebSearch
	ToolUsageXSearch           = outcome.ToolUsageXSearch
	WorkAudioDurationMillis    = outcome.WorkAudioDurationMillis
	WorkImagesGenerated        = outcome.WorkImagesGenerated
	WorkInputCharacters        = outcome.WorkInputCharacters
	WorkVideoDurationMillis    = outcome.WorkVideoDurationMillis
	WorkVideosGenerated        = outcome.WorkVideosGenerated
)
