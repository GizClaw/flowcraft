package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// fakeMessages serves one commit per conversation and observes how many
// conversations are being listed at the same time.
type fakeMessages struct {
	commits  map[string][]msgsource.Commit
	failOn   string
	delay    time.Duration
	inFlight atomic.Int64
	peak     atomic.Int64
}

func (source *fakeMessages) ListConversations(context.Context, corememory.Scope) ([]string, error) {
	ids := make([]string, 0, len(source.commits))
	for id := range source.commits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (source *fakeMessages) ListCommits(_ context.Context, _ corememory.Scope, conversationID string, options msgsource.ListCommitOptions) ([]msgsource.Commit, error) {
	current := source.inFlight.Add(1)
	defer source.inFlight.Add(-1)
	for {
		peak := source.peak.Load()
		if current <= peak || source.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	if source.delay > 0 {
		time.Sleep(source.delay)
	}
	if conversationID == source.failOn {
		return nil, errors.New("boom")
	}
	commits := source.commits[conversationID]
	filtered := make([]msgsource.Commit, 0, len(commits))
	for _, commit := range commits {
		if commit.Version > options.AfterVersion {
			filtered = append(filtered, commit)
		}
	}
	return filtered, nil
}

func (source *fakeMessages) List(context.Context, corememory.Scope, string, msgsource.ListOptions) ([]msgsource.Record, error) {
	return nil, nil
}

// memoryCheckpoints keeps watermarks in memory.
type memoryCheckpoints struct {
	mu     sync.Mutex
	values map[string]uint64
}

func newMemoryCheckpoints() *memoryCheckpoints {
	return &memoryCheckpoints{values: map[string]uint64{}}
}

func (store *memoryCheckpoints) LoadWatermark(_ context.Context, scope corememory.Scope, streamKind, streamID, digest string) (SourceWatermark, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	cursor, ok := store.values[checkpointKey(scope, streamKind, streamID, digest)]
	if !ok {
		return SourceWatermark{}, false, nil
	}
	return SourceWatermark{Cursor: cursor}, true, nil
}

func (store *memoryCheckpoints) SaveWatermark(_ context.Context, watermark SourceWatermark) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.values[checkpointKey(watermark.Scope, watermark.StreamKind, watermark.StreamID, watermark.PolicyDigest)] = watermark.Cursor
	return nil
}

func (store *memoryCheckpoints) snapshot() map[string]uint64 {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make(map[string]uint64, len(store.values))
	for key, value := range store.values {
		out[key] = value
	}
	return out
}

func checkpointKey(scope corememory.Scope, streamKind, streamID, digest string) string {
	return streamKind + "/" + streamID + "/" + digest
}

// countingIndexer counts applied deltas so a test can prove every conversation
// reached the projection lanes.
type countingIndexer struct {
	mu    sync.Mutex
	total int
}

func (indexer *countingIndexer) ApplyDelta(context.Context, component.ProjectionDelta) error {
	indexer.mu.Lock()
	defer indexer.mu.Unlock()
	indexer.total++
	return nil
}

func (indexer *countingIndexer) count() int {
	indexer.mu.Lock()
	defer indexer.mu.Unlock()
	return indexer.total
}

func newTestProcessor(t *testing.T, messages MessageReader, checkpoints CheckpointStore, concurrency int) (*Processor, *countingIndexer, *factview.FactStore) {
	t.Helper()
	return newTestProcessorWith(t, messages, checkpoints, concurrency, nil)
}

func newTestProcessorWith(t *testing.T, messages MessageReader, checkpoints CheckpointStore, concurrency int, deriver component.Deriver) (*Processor, *countingIndexer, *factview.FactStore) {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factview.NewFactStore(logStore, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	indexer := &countingIndexer{}
	processor, err := NewProcessor(Config{
		Messages: messages, Facts: facts, Deriver: deriver, Checkpoints: checkpoints,
		Projection: "test", PolicyDigest: "digest-test", Concurrency: concurrency,
		Indexers: []ProjectionIndexer{{Name: "test", Indexer: indexer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return processor, indexer, facts
}

// hashDeriver derives one fact per commit from the commit text. It is
// deliberately content-addressed and free of ordering state, so two runs that
// derive the same conversations must produce the same facts no matter how the
// worker scheduled them.
type hashDeriver struct{}

func (hashDeriver) Derive(_ context.Context, source component.Artifact) ([]component.Artifact, error) {
	text := strings.TrimSpace(source.Content.Text())
	content := coremessage.NewTextContent("fact for: " + text)
	canonical := factview.CanonicalHash(content.Text())
	sum := sha256.Sum256([]byte("test-derive\x00" + canonical))
	hash := hex.EncodeToString(sum[:])
	id := "fact-" + hash[:16]
	metadata := corememory.Metadata{}
	for key, value := range source.Metadata {
		metadata[key] = value
	}
	metadata["canonical_hash"] = canonical
	metadata["transform_signature"] = "test-derive-v1"
	metadata["source_digest"] = hash[:12]
	return []component.Artifact{{
		Kind: chat.KindFact, ID: id, Content: content,
		Sources: append([]corememory.SourceRef(nil), source.Sources...), Metadata: metadata,
	}}, nil
}

// factSnapshot is the derived-fact view a test compares across runs.
type factSnapshot map[string][]string

func captureFacts(t *testing.T, facts *factview.FactStore, scope corememory.Scope, conversations []string) factSnapshot {
	t.Helper()
	snapshot := factSnapshot{}
	for _, conversationID := range conversations {
		values, err := facts.List(context.Background(), scope, conversationID, factview.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		lines := make([]string, 0, len(values))
		for _, fact := range values {
			lines = append(lines, fact.ID+" | "+fact.Content.Text())
		}
		sort.Strings(lines)
		snapshot[conversationID] = lines
	}
	return snapshot
}

func testCommits(conversations ...string) map[string][]msgsource.Commit {
	commits := make(map[string][]msgsource.Commit, len(conversations))
	for _, conversationID := range conversations {
		commits[conversationID] = []msgsource.Commit{{
			ID: "commit-" + conversationID, Scope: corememory.Scope{RuntimeID: "memories"}, Version: 1,
			ConversationID: conversationID,
			Records: []msgsource.Record{{
				ID: "msg-" + conversationID, ConversationID: conversationID, Seq: 1,
				Message: coremessage.NewTextMessage(coremessage.RoleUser, "hello from "+conversationID),
			}},
		}}
	}
	return commits
}

// TestProcessScopeParallelDerivationMatchesSequential pins the derive.concurrency
// contract: every conversation is derived exactly once, each keeps its own
// watermark at its last commit, and the projection lanes see the same deltas.
// Only the schedule differs.
func TestProcessScopeParallelDerivationMatchesSequential(t *testing.T) {
	scope := corememory.Scope{RuntimeID: "memories"}
	conversations := []string{"conv-a", "conv-b", "conv-c", "conv-d"}

	sequentialMessages := &fakeMessages{commits: testCommits(conversations...)}
	sequentialCheckpoints := newMemoryCheckpoints()
	sequential, sequentialIndexer, _ := newTestProcessor(t, sequentialMessages, sequentialCheckpoints, 1)
	if err := sequential.ProcessScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}

	parallelMessages := &fakeMessages{commits: testCommits(conversations...), delay: 5 * time.Millisecond}
	parallelCheckpoints := newMemoryCheckpoints()
	parallel, parallelIndexer, _ := newTestProcessor(t, parallelMessages, parallelCheckpoints, 4)
	if err := parallel.ProcessScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}

	if peak := parallelMessages.peak.Load(); peak < 2 || peak > 4 {
		t.Fatalf("conversations in flight peaked at %d, want 2..4", peak)
	}
	if peak := sequentialMessages.peak.Load(); peak != 1 {
		t.Fatalf("sequential run listed %d conversations at once", peak)
	}
	if !reflect.DeepEqual(sequentialCheckpoints.snapshot(), parallelCheckpoints.snapshot()) {
		t.Fatalf("watermarks differ:\n sequential: %#v\n parallel:   %#v",
			sequentialCheckpoints.snapshot(), parallelCheckpoints.snapshot())
	}
	if sequentialIndexer.count() != len(conversations) || parallelIndexer.count() != len(conversations) {
		t.Fatalf("deltas applied: sequential=%d parallel=%d, want %d each",
			sequentialIndexer.count(), parallelIndexer.count(), len(conversations))
	}
}

// TestProcessScopeParallelDerivesIdenticalFacts is the byte-equality proof for
// derive.concurrency: with a deterministic deriver, deriving the same
// conversations one at a time and four at a time must land exactly the same
// facts (same content-derived ids, same text) under the same conversation, and
// none of them may leak into a neighbouring conversation.
func TestProcessScopeParallelDerivesIdenticalFacts(t *testing.T) {
	scope := corememory.Scope{RuntimeID: "memories"}
	conversations := []string{"conv-a", "conv-b", "conv-c", "conv-d"}

	sequentialCheckpoints := newMemoryCheckpoints()
	sequential, _, sequentialFacts := newTestProcessorWith(t,
		&fakeMessages{commits: testCommits(conversations...)}, sequentialCheckpoints, 1, hashDeriver{})
	if err := sequential.ProcessScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}

	parallelCheckpoints := newMemoryCheckpoints()
	parallel, _, parallelFacts := newTestProcessorWith(t,
		&fakeMessages{commits: testCommits(conversations...), delay: 5 * time.Millisecond},
		parallelCheckpoints, 4, hashDeriver{})
	if err := parallel.ProcessScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}

	sequentialSnapshot := captureFacts(t, sequentialFacts, scope, conversations)
	parallelSnapshot := captureFacts(t, parallelFacts, scope, conversations)
	if !reflect.DeepEqual(sequentialSnapshot, parallelSnapshot) {
		t.Fatalf("derived facts differ:\n sequential: %#v\n parallel:   %#v", sequentialSnapshot, parallelSnapshot)
	}
	for _, conversationID := range conversations {
		if len(sequentialSnapshot[conversationID]) != 1 {
			t.Fatalf("conversation %s derived %d facts, want 1: %#v",
				conversationID, len(sequentialSnapshot[conversationID]), sequentialSnapshot[conversationID])
		}
		for _, line := range sequentialSnapshot[conversationID] {
			if !strings.Contains(line, "fact for:") {
				t.Fatalf("unexpected derived fact %q", line)
			}
		}
	}
	// Cross-conversation contamination would show up as an identical fact id
	// under two conversations; the ids are content-derived, so compare them.
	seen := map[string]string{}
	for _, conversationID := range conversations {
		for _, line := range sequentialSnapshot[conversationID] {
			id, _, _ := strings.Cut(line, " | ")
			if owner, ok := seen[id]; ok {
				t.Fatalf("fact %s appears under both %s and %s", id, owner, conversationID)
			}
			seen[id] = conversationID
		}
	}
}

// TestProcessScopeParallelKeepsFailingConversationIsolated checks that one bad
// conversation neither blocks the others nor advances its own watermark.
func TestProcessScopeParallelKeepsFailingConversationIsolated(t *testing.T) {
	scope := corememory.Scope{RuntimeID: "memories"}
	conversations := []string{"conv-a", "conv-b", "conv-c", "conv-d"}
	messages := &fakeMessages{commits: testCommits(conversations...), failOn: "conv-b"}
	checkpoints := newMemoryCheckpoints()
	processor, indexer, _ := newTestProcessor(t, messages, checkpoints, 4)

	err := processor.ProcessScope(context.Background(), scope)
	if err == nil {
		t.Fatal("a failing conversation was not reported")
	}
	if !strings.Contains(err.Error(), "conv-b") {
		t.Fatalf("failure does not name the conversation: %v", err)
	}
	if indexer.count() != len(conversations)-1 {
		t.Fatalf("deltas applied = %d, want %d", indexer.count(), len(conversations)-1)
	}
	snapshot := checkpoints.snapshot()
	for _, conversationID := range conversations {
		key := checkpointKey(scope, streamKindMessages, conversationID, "digest-test")
		if conversationID == "conv-b" {
			if _, ok := snapshot[key]; ok {
				t.Fatal("the failing conversation advanced its watermark")
			}
			continue
		}
		if snapshot[key] != 1 {
			t.Fatalf("conversation %s watermark = %d, want 1", conversationID, snapshot[key])
		}
	}
}
