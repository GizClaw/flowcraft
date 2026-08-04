package qwen

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// ledger tracks one compile's active fields and the dispositions the
// compiler assigns them, so a request never loses a setting silently:
// every active field ends the compile as Native, Dropped, or Rejected.
type ledger struct {
	operation inference.Operation
	active    []inference.FieldID
	rejected  map[inference.FieldID]string
	dropped   map[inference.FieldID]string
	order     []inference.FieldID // rejection order, deterministic
}

func newLedger(
	operation inference.Operation,
	active []inference.FieldID,
) *ledger {
	return &ledger{
		operation: operation,
		active:    append([]inference.FieldID(nil), active...),
		rejected:  make(map[inference.FieldID]string),
		dropped:   make(map[inference.FieldID]string),
	}
}

func (l *ledger) reject(field inference.FieldID, reason string) {
	if _, exists := l.rejected[field]; !exists {
		l.order = append(l.order, field)
		l.rejected[field] = reason
	}
}

// drop records an intentional discard that keeps the compile successful.
// Rejection wins when both land on one field: a failed compile reports the
// rejection.
func (l *ledger) drop(field inference.FieldID, reason string) {
	if _, rejected := l.rejected[field]; rejected {
		return
	}
	if _, exists := l.dropped[field]; !exists {
		l.dropped[field] = reason
	}
}

// report renders the compile report: every active field carries exactly one
// disposition — Rejected, then Dropped, otherwise Native.
func (l *ledger) report() inference.CompileReport {
	decisions := make([]inference.Decision, 0, len(l.active))
	for _, field := range l.active {
		if reason, rejected := l.rejected[field]; rejected {
			decisions = append(decisions, inference.Decision{
				Field:       field,
				Disposition: inference.Rejected,
				Reason:      reason,
			})
			continue
		}
		if reason, dropped := l.dropped[field]; dropped {
			decisions = append(decisions, inference.Decision{
				Field:       field,
				Disposition: inference.Dropped,
				Reason:      reason,
			})
			continue
		}
		decisions = append(decisions, inference.Decision{
			Field:       field,
			Disposition: inference.Native,
		})
	}
	return inference.CompileReport{
		Operation: l.operation,
		Decisions: decisions,
	}
}

// err builds the structured compiler rejection. The first rejected field in
// rejection order becomes the error field; extension rejections classify as
// InvalidExtension, everything else as UnsupportedFeature.
func (l *ledger) err() error {
	field := l.order[0]
	kind := inference.UnsupportedFeature
	if strings.HasPrefix(string(field), "extension.") {
		kind = inference.InvalidExtension
	}
	return inference.NewError(
		kind,
		l.operation,
		field,
		fmt.Errorf("qwen: %s", l.rejected[field]),
	)
}

var contextPartFields = map[message.PartKind]inference.FieldID{
	message.PartText:       inference.FieldGenerateContextText,
	message.PartImage:      inference.FieldGenerateContextImage,
	message.PartAudio:      inference.FieldGenerateContextAudio,
	message.PartVideo:      inference.FieldGenerateContextVideo,
	message.PartFile:       inference.FieldGenerateContextFile,
	message.PartData:       inference.FieldGenerateContextData,
	message.PartToolCall:   inference.FieldGenerateContextToolCall,
	message.PartToolResult: inference.FieldGenerateContextToolResult,
	message.PartReasoning:  inference.FieldGenerateContextReasoning,
}

var inputPartFields = map[message.PartKind]inference.FieldID{
	message.PartText:       inference.FieldGenerateInputText,
	message.PartImage:      inference.FieldGenerateInputImage,
	message.PartAudio:      inference.FieldGenerateInputAudio,
	message.PartVideo:      inference.FieldGenerateInputVideo,
	message.PartFile:       inference.FieldGenerateInputFile,
	message.PartData:       inference.FieldGenerateInputData,
	message.PartToolCall:   inference.FieldGenerateInputToolCall,
	message.PartToolResult: inference.FieldGenerateInputToolResult,
	message.PartReasoning:  inference.FieldGenerateInputReasoning,
}

var embedPartFields = map[message.PartKind]inference.FieldID{
	message.PartText:       inference.FieldEmbedItemText,
	message.PartImage:      inference.FieldEmbedItemImage,
	message.PartAudio:      inference.FieldEmbedItemAudio,
	message.PartVideo:      inference.FieldEmbedItemVideo,
	message.PartFile:       inference.FieldEmbedItemFile,
	message.PartData:       inference.FieldEmbedItemData,
	message.PartToolCall:   inference.FieldEmbedItemToolCall,
	message.PartToolResult: inference.FieldEmbedItemToolResult,
}
