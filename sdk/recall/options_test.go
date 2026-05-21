package recall_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/text/timex"
)

type externalTelemetryHook struct{}

func (externalTelemetryHook) OnStage(recall.StageDiagnostic) {}

type externalTimeParser struct{}

func (externalTimeParser) Parse(string, time.Time) (*timex.Match, error) { return nil, nil }

type externalEntityExtractor struct{}

func (externalEntityExtractor) ExtractEntities(string, []recall.EntitySnapshot) []string {
	return []string{"external"}
}

type externalEvidenceStore struct{}

func (externalEvidenceStore) Append(context.Context, recall.Scope, string, []recall.EvidenceRef) error {
	return nil
}
func (externalEvidenceStore) Get(context.Context, recall.Scope, string) (recall.EvidenceRef, error) {
	return recall.EvidenceRef{}, nil
}
func (externalEvidenceStore) ListByFact(context.Context, recall.Scope, string) ([]recall.EvidenceRef, error) {
	return nil, nil
}
func (externalEvidenceStore) ListFactIDs(context.Context, recall.Scope) ([]string, error) {
	return nil, nil
}
func (externalEvidenceStore) ForgetByFact(context.Context, recall.Scope, []string) error {
	return nil
}
func (externalEvidenceStore) Close() error { return nil }

func TestPublicOptionsDoNotRequireInternalImports(t *testing.T) {
	mem, err := recall.New(
		recall.WithRetrievalIndex(retrievalmem.New()),
		recall.WithEvidenceStore(recall.NewMemoryEvidenceStore()),
		recall.WithTelemetryHook(externalTelemetryHook{}),
		recall.WithGraphEnabled(true),
		recall.WithTimeParser(externalTimeParser{}),
		recall.WithEntityExtractor(externalEntityExtractor{}),
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

func TestExternalEvidenceStoreCanBeProvided(t *testing.T) {
	mem, err := recall.New(recall.WithEvidenceStore(externalEvidenceStore{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}
}
