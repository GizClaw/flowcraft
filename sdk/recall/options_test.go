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

type externalTemporalStore struct{}

func (externalTemporalStore) Append(context.Context, []recall.TemporalFact) error { return nil }
func (externalTemporalStore) Get(context.Context, recall.Scope, string) (recall.TemporalFact, error) {
	return recall.TemporalFact{}, recall.ErrStoreNotFound
}
func (externalTemporalStore) List(context.Context, recall.Scope, recall.ListQuery) ([]recall.TemporalFact, error) {
	return nil, nil
}
func (externalTemporalStore) FindByMergeKey(context.Context, recall.Scope, string) ([]recall.TemporalFact, error) {
	return nil, nil
}
func (externalTemporalStore) FindSupersededBy(context.Context, recall.Scope, string) ([]recall.TemporalFact, error) {
	return nil, nil
}
func (externalTemporalStore) FindByRevisionSource(context.Context, recall.Scope, string) ([]recall.TemporalFact, error) {
	return nil, nil
}
func (externalTemporalStore) FindByOriginRequestID(context.Context, recall.Scope, string) ([]recall.TemporalFact, error) {
	return nil, nil
}
func (externalTemporalStore) UpdateValidity(context.Context, recall.Scope, string, time.Time, string) error {
	return nil
}
func (externalTemporalStore) ReopenValidity(context.Context, recall.Scope, string, string) error {
	return nil
}
func (externalTemporalStore) Delete(context.Context, recall.Scope, []string) error { return nil }
func (externalTemporalStore) UpdateFeedback(context.Context, recall.Scope, string, float64, float64) error {
	return nil
}
func (externalTemporalStore) MarkClosed(context.Context, recall.Scope, string, bool) error {
	return nil
}
func (externalTemporalStore) ListByID(context.Context, recall.Scope, string) ([]recall.TemporalFact, error) {
	return nil, nil
}
func (externalTemporalStore) DeleteByScope(context.Context, recall.Scope) (int, error) {
	return 0, nil
}
func (externalTemporalStore) Close() error { return nil }

type externalSideEffectOutbox struct{}

func (externalSideEffectOutbox) Enqueue(context.Context, recall.SideEffectJob) error { return nil }
func (externalSideEffectOutbox) Claim(context.Context, recall.SideEffectClaimOptions) ([]recall.SideEffectJob, error) {
	return nil, nil
}
func (externalSideEffectOutbox) Complete(context.Context, string, string, recall.SideEffectResult) error {
	return nil
}
func (externalSideEffectOutbox) Fail(context.Context, string, string, recall.SideEffectFailure) error {
	return nil
}
func (externalSideEffectOutbox) Cancel(context.Context, string) error { return nil }
func (externalSideEffectOutbox) CancelScope(context.Context, recall.Scope) (int, error) {
	return 0, nil
}
func (externalSideEffectOutbox) PurgeScope(context.Context, recall.Scope) (int, error) {
	return 0, nil
}
func (externalSideEffectOutbox) Stats(context.Context, recall.Scope, time.Time) (recall.SideEffectOutboxStats, error) {
	return recall.SideEffectOutboxStats{}, nil
}

type externalAsyncSemanticQueue struct{}

func (externalAsyncSemanticQueue) Enqueue(context.Context, recall.AsyncSemanticJob) (recall.AsyncSemanticReceipt, error) {
	return recall.AsyncSemanticReceipt{}, nil
}
func (externalAsyncSemanticQueue) Cancel(context.Context, string) error { return nil }
func (externalAsyncSemanticQueue) CancelScope(context.Context, recall.Scope) (int, error) {
	return 0, nil
}
func (externalAsyncSemanticQueue) PurgeScope(context.Context, recall.Scope) (int, error) {
	return 0, nil
}
func (externalAsyncSemanticQueue) CancelMatchingEpisodes(context.Context, recall.Scope, []string) (int, error) {
	return 0, nil
}
func (externalAsyncSemanticQueue) Claim(context.Context, recall.AsyncSemanticClaimOptions) ([]recall.AsyncSemanticJob, error) {
	return nil, nil
}
func (externalAsyncSemanticQueue) Complete(context.Context, string, string, recall.AsyncSemanticResult) error {
	return nil
}
func (externalAsyncSemanticQueue) Fail(context.Context, string, string, recall.AsyncSemanticFailure) error {
	return nil
}
func (externalAsyncSemanticQueue) Stats(context.Context, recall.AsyncSemanticStatsFilter) (recall.AsyncSemanticStats, error) {
	return recall.AsyncSemanticStats{}, nil
}

var (
	_ recall.TemporalStore      = externalTemporalStore{}
	_ recall.SideEffectOutbox   = externalSideEffectOutbox{}
	_ recall.AsyncSemanticQueue = externalAsyncSemanticQueue{}
)

func TestPublicOptionsDoNotRequireInternalImports(t *testing.T) {
	mem, err := recall.New(
		recall.WithRetrievalIndex(retrievalmem.New()),
		recall.WithTemporalStore(externalTemporalStore{}),
		recall.WithEvidenceStore(recall.NewMemoryEvidenceStore()),
		recall.WithSideEffectOutbox(externalSideEffectOutbox{}),
		recall.WithAsyncSemanticQueue(externalAsyncSemanticQueue{}),
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

func TestPublicDurableAdapterHelpersDoNotRequireInternalImports(t *testing.T) {
	job := recall.AsyncSemanticJob{
		RequestID:      "req-1",
		TurnsSnapshot:  []recall.TurnContext{{Text: "secret"}},
		RecentMessages: []recall.Message{{Text: "context"}},
	}
	cloned := recall.CloneAsyncSemanticJob(job)
	recall.ScrubAsyncSemanticJobPII(&cloned)
	if len(cloned.TurnsSnapshot) != 0 || len(cloned.RecentMessages) != 0 {
		t.Fatalf("async semantic scrub retained PII snapshots: %+v", cloned)
	}

	side := recall.SideEffectJob{
		Kind:  recall.SideEffectProjectRequired,
		Facts: []recall.TemporalFact{{ID: "f1", Kind: recall.FactNote, Content: "secret"}},
	}
	recall.ScrubSideEffectJobPayload(&side)
	if got := side.Facts[0].Content; got != "" {
		t.Fatalf("side-effect scrub retained fact content %q", got)
	}

	_ = recall.AsyncSemanticFailure{ErrClass: recall.ErrClassTransient}
	_ = recall.SideEffectFailure{ErrClass: recall.ErrClassPermanent}
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
