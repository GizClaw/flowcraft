package recall_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

type externalTelemetryHook struct{}

func (externalTelemetryHook) OnProjection(recall.ProjectionEvent) {}
func (externalTelemetryHook) OnDrift(recall.DriftEvent)           {}

func TestPublicOptionsDoNotRequireInternalImports(t *testing.T) {
	mem, err := recall.New(
		recall.WithRetrievalIndex(retrievalmem.New()),
		recall.WithTelemetryHook(externalTelemetryHook{}),
		recall.WithGraphEnabled(true),
		recall.WithLLMExtractor(nil,
			recall.WithLLMExtractorSystemPrompt("extract facts"),
			recall.WithLLMExtractorSchemaName("recall_facts"),
			recall.WithLLMExtractorTemperature(0.1),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}
}
