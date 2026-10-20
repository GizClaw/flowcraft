package inference

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference/usage"
)

// The aliases exist so callers keep compiling; they are only honest if a value
// crosses between the two spellings with no conversion, in both directions.
// Blank declarations pin exactly that, and pin the classifier constants' types
// while we are here.
var (
	_ Usage       = usage.Usage{}
	_ usage.Usage = Usage{}

	_ Money       = usage.Money{}
	_ usage.Money = Money{}

	_ Modality       = usage.ModalityAudio
	_ usage.Modality = ModalityAudio

	_ ToolUsageKind       = usage.ToolUsageMCP
	_ usage.ToolUsageKind = ToolUsageMCP
)

func TestUsageAliasMethodsAreShared(t *testing.T) {
	// A method set is part of the type, so Add accepting the other spelling's
	// value is the check that these are one type rather than two look-alikes.
	money := Money{Currency: "usd", Units: 1, Scale: 4}
	sum, ok := money.Add(usage.Money{Currency: "usd", Units: 2, Scale: 4})
	if !ok || sum.Units != 3 {
		t.Fatalf("Add across spellings = %+v, ok=%v, want 3 units", sum, ok)
	}
}
