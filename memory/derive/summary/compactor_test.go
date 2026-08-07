package summary

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/storage"
	summaryview "github.com/GizClaw/flowcraft/memory/views/summary"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestDefaultConfig(t *testing.T) {
	got := DefaultConfig()
	if got.ChunkSize != 10 || got.CondenseThreshold != 6 || got.GroupSize != 3 || got.MaxDepth != 4 {
		t.Fatalf("defaults=%+v", got)
	}
}

func TestCompactorBuildsFourLevelsRespectsThresholdDepthAndReplay(t *testing.T) {
	ctx := context.Background()
	logStore, _ := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	kvStore, _ := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	store, _ := summaryview.NewSummaryStore(logStore, kvStore, summaryview.WithClock(func() time.Time {
		return time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	}))
	compactor, err := New(Config{ChunkSize: 2, CondenseThreshold: 2, GroupSize: 2, MaxDepth: 4}, store, echoSummarizer{})
	if err != nil {
		t.Fatal(err)
	}
	request := CompactRequest{Scope: testScope(), ConversationID: "conversation", GenerationID: "generation", Inputs: inputs(8)}
	first, err := compactor.Compact(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[summaryview.Level]int{}
	for _, record := range first {
		counts[record.Level]++
	}
	if counts[summaryview.L0] != 8 || counts[summaryview.L1] != 4 ||
		counts[summaryview.L2] != 2 || counts[summaryview.L3] != 1 {
		t.Fatalf("level counts=%v", counts)
	}
	replayed, err := compactor.Compact(ctx, request)
	if err != nil || len(replayed) != len(first) {
		t.Fatalf("replay len=%d err=%v", len(replayed), err)
	}
	for index := range first {
		if first[index].ID != replayed[index].ID {
			t.Fatalf("unstable id at %d: %q != %q", index, first[index].ID, replayed[index].ID)
		}
	}

	shallow, _ := New(Config{ChunkSize: 2, CondenseThreshold: 2, GroupSize: 2, MaxDepth: 2},
		mustStore(t), echoSummarizer{})
	records, err := shallow.Compact(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Level > summaryview.L1 {
			t.Fatalf("MaxDepth produced %s", record.Level)
		}
	}
	belowThreshold, _ := New(Config{ChunkSize: 2, CondenseThreshold: 5, GroupSize: 2, MaxDepth: 4},
		mustStore(t), echoSummarizer{})
	records, err = belowThreshold.Compact(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Level > summaryview.L1 {
			t.Fatalf("below-threshold compaction produced %s", record.Level)
		}
	}
}

func TestCompactorIncrementallyReusesStablePrefix(t *testing.T) {
	ctx := context.Background()
	base := mustStore(t)
	store := &countingStore{Store: base}
	summarizer := &countingSummarizer{}
	compactor, _ := New(DefaultConfig(), store, summarizer)
	first, err := compactor.Compact(ctx, CompactRequest{
		Scope: testScope(), ConversationID: "conversation", GenerationID: "generation-100", Inputs: inputs(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	firstIDs := recordIDSet(first)
	summarizer.calls = 0
	store.adds = 0
	second, err := compactor.Compact(ctx, CompactRequest{
		Scope: testScope(), ConversationID: "conversation", GenerationID: "generation-101", Inputs: inputs(101),
	})
	if err != nil {
		t.Fatal(err)
	}
	if summarizer.calls > DefaultConfig().MaxDepth {
		t.Fatalf("incremental summarizer calls=%d, max=%d", summarizer.calls, DefaultConfig().MaxDepth)
	}
	if store.adds > DefaultConfig().MaxDepth {
		t.Fatalf("incremental writes=%d, max=%d", store.adds, DefaultConfig().MaxDepth)
	}
	reused := 0
	for _, record := range second {
		if _, ok := firstIDs[record.ID]; ok {
			reused++
		}
	}
	if reused < len(first)-DefaultConfig().MaxDepth {
		t.Fatalf("reused=%d first=%d second=%d", reused, len(first), len(second))
	}
	summarizer.calls = 0
	store.adds = 0
	if _, err := compactor.Compact(ctx, CompactRequest{
		Scope: testScope(), ConversationID: "conversation", GenerationID: "generation-replay", Inputs: inputs(101),
	}); err != nil {
		t.Fatal(err)
	}
	if summarizer.calls != 0 || store.adds != 0 {
		t.Fatalf("same-input replay calls=%d writes=%d", summarizer.calls, store.adds)
	}
}

func TestCompactorIncrementalChunkBoundariesAndThresholdCascade(t *testing.T) {
	ctx := context.Background()
	store := mustStore(t)
	summarizer := &countingSummarizer{}
	compactor, _ := New(Config{ChunkSize: 10, CondenseThreshold: 2, GroupSize: 2, MaxDepth: 4}, store, summarizer)
	var prior map[string]struct{}
	for _, count := range []int{9, 10, 11, 20, 21} {
		summarizer.calls = 0
		records, err := compactor.Compact(ctx, CompactRequest{
			Scope: testScope(), ConversationID: "conversation",
			GenerationID: fmt.Sprintf("generation-%d", count), Inputs: inputs(count),
		})
		if err != nil {
			t.Fatalf("count=%d: %v", count, err)
		}
		if prior != nil && summarizer.calls > 3 {
			t.Fatalf("count=%d incremental calls=%d", count, summarizer.calls)
		}
		prior = recordIDSet(records)
	}
}

func TestCompactorReplaysRecordsAfterManifestPublishCrash(t *testing.T) {
	ctx := context.Background()
	base := mustStore(t)
	store := &failPublishStore{countingStore: &countingStore{Store: base}}
	summarizer := &countingSummarizer{}
	compactor, _ := New(DefaultConfig(), store, summarizer)
	request := CompactRequest{
		Scope: testScope(), ConversationID: "conversation", GenerationID: "generation", Inputs: inputs(11),
	}
	if _, err := compactor.Compact(ctx, request); err == nil {
		t.Fatal("manifest publish failure was not surfaced")
	}
	summarizer.calls = 0
	store.adds = 0
	records, err := compactor.Compact(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 || summarizer.calls != 0 || store.adds != 0 {
		t.Fatalf("replay records=%d calls=%d writes=%d", len(records), summarizer.calls, store.adds)
	}
}

func TestCompactorRecursiveHalveFallbackAndCancel(t *testing.T) {
	failing := &selectiveSummarizer{failAbove: 1, failSingle: true}
	compactor, _ := New(Config{ChunkSize: 4, CondenseThreshold: 99, GroupSize: 2, MaxDepth: 4},
		mustStore(t), failing)
	records, err := compactor.Compact(context.Background(), CompactRequest{
		Scope: testScope(), ConversationID: "conversation", GenerationID: "generation", Inputs: inputs(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	var l1 []summaryview.Record
	for _, record := range records {
		if record.Level == summaryview.L1 {
			l1 = append(l1, record)
		}
	}
	if len(l1) != 4 || !strings.Contains(l1[0].Text, "fact 00") {
		t.Fatalf("fallback summaries=%#v", l1)
	}
	if failing.calls < 7 {
		t.Fatalf("recursive calls=%d", failing.calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceling, _ := New(DefaultConfig(), mustStore(t), cancelSummarizer{})
	records, err = canceling.Compact(ctx, CompactRequest{
		Scope: testScope(), ConversationID: "conversation", GenerationID: "generation", Inputs: inputs(1),
	})
	if !errors.Is(err, context.Canceled) || len(records) != 0 {
		t.Fatalf("cancel records=%d err=%v", len(records), err)
	}
}

type echoSummarizer struct{}

func (echoSummarizer) Summarize(_ context.Context, request SummarizeRequest) (string, error) {
	return fmt.Sprintf("%s:%s", request.Level, strings.Join(request.Texts, "|")), nil
}

type selectiveSummarizer struct {
	failAbove  int
	failSingle bool
	calls      int
}

func (value *selectiveSummarizer) Summarize(_ context.Context, request SummarizeRequest) (string, error) {
	value.calls++
	if len(request.Texts) > value.failAbove || (len(request.Texts) == 1 && value.failSingle) {
		return "", errors.New("model failed")
	}
	return strings.Join(request.Texts, " "), nil
}

type cancelSummarizer struct{}

func (cancelSummarizer) Summarize(context.Context, SummarizeRequest) (string, error) {
	return "", context.Canceled
}

type countingSummarizer struct{ calls int }

func (value *countingSummarizer) Summarize(_ context.Context, request SummarizeRequest) (string, error) {
	value.calls++
	return fmt.Sprintf("%s:%s", request.Level, strings.Join(request.Texts, "|")), nil
}

type countingStore struct {
	Store
	adds int
}

func (store *countingStore) Add(ctx context.Context, request summaryview.AddRequest) (summaryview.Record, error) {
	store.adds++
	return store.Store.Add(ctx, request)
}

type failPublishStore struct {
	*countingStore
	failed bool
}

func (store *failPublishStore) PublishActive(ctx context.Context, manifest summaryview.Manifest) error {
	if !store.failed {
		store.failed = true
		return errors.New("manifest publish failed")
	}
	return store.Store.PublishActive(ctx, manifest)
}

func recordIDSet(records []summaryview.Record) map[string]struct{} {
	result := make(map[string]struct{}, len(records))
	for _, record := range records {
		result[record.ID] = struct{}{}
	}
	return result
}

func inputs(count int) []Input {
	result := make([]Input, count)
	for index := range result {
		result[index] = Input{
			ID: fmt.Sprintf("fact-%02d", index), Text: fmt.Sprintf("fact %02d", index),
			Topics: []string{"topic"}, SourceRefs: []sdkmemory.SourceRef{{
				Kind: sdkmemory.SourceMessage, ID: fmt.Sprintf("conversation/message-%02d", index),
				Revision: fmt.Sprint(index + 1),
			}},
			CoverageRange: summaryview.CoverageRange{StartSeq: uint64(index + 1), EndSeq: uint64(index + 1)},
		}
	}
	return result
}

func testScope() sdkmemory.Scope {
	return sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
}

func mustStore(t *testing.T) Store {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	store, err := summaryview.NewSummaryStore(logStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

var _ = sdkmessage.Content{}
