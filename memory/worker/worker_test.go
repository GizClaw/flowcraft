package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/derive"
	summaryderive "github.com/GizClaw/flowcraft/memory/derive/summary"
	"github.com/GizClaw/flowcraft/memory/lines/chat"
	"github.com/GizClaw/flowcraft/memory/lines/knowledge"
	"github.com/GizClaw/flowcraft/memory/sources"
	docsource "github.com/GizClaw/flowcraft/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/memory/sources/message"
	docview "github.com/GizClaw/flowcraft/memory/views/document"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/memory/views/summary"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var (
	scopeA = sdkmemory.Scope{RuntimeID: "runtime", UserID: "alice"}
	scopeB = sdkmemory.Scope{RuntimeID: "runtime", UserID: "bob"}
)

func TestSourceFailureRemainsRetryableAndBranchesAreIsolated(t *testing.T) {
	fixture := newFixture(t)
	fixture.chatDeriver.setFail(true)
	fixture.putTurn(t, scopeA, "conversation", "turn", "remember me")
	fixture.putDocument(t, scopeA, "dataset", "document", "knowledge")

	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err == nil {
		t.Fatal("first scan error = nil")
	}
	chunks, err := fixture.documentViews.List(context.Background(), scopeA, "dataset", "document", docview.ListOptions{})
	if err != nil || len(leafChunks(chunks)) != 1 {
		t.Fatalf("knowledge branch records=%d err=%v", len(chunks), err)
	}
	facts, _ := fixture.facts.List(context.Background(), scopeA, "conversation", factview.ListOptions{})
	if len(facts) != 0 {
		t.Fatalf("failed chat branch wrote %d facts", len(facts))
	}

	fixture.chatDeriver.setFail(false)
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	facts, err = fixture.facts.List(context.Background(), scopeA, "conversation", factview.ListOptions{})
	if err != nil || len(facts) != 1 {
		t.Fatalf("retried facts=%d err=%v", len(facts), err)
	}
	records, _ := fixture.messages.List(context.Background(), scopeA, "conversation", msgsource.ListOptions{})
	if len(records) != 1 {
		t.Fatalf("source retry duplicated records: %d", len(records))
	}
}

func TestCheckpointFailureRedoesWithoutDuplicatingFactAndReopens(t *testing.T) {
	fixture := newFixture(t)
	fixture.putTurn(t, scopeA, "conversation", "turn", "stable")
	failing := &failCompleteCheckpoint{CheckpointStore: fixture.checkpoints}
	fixture.processor = fixture.makeProcessor(t, failing, fixture.indexers)

	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err == nil {
		t.Fatal("checkpoint failure was not surfaced")
	}
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	facts, _ := fixture.facts.List(context.Background(), scopeA, "conversation", factview.ListOptions{})
	if len(facts) != 1 {
		t.Fatalf("redo produced %d facts", len(facts))
	}
	if fixture.chatDeriver.calls() != 2 {
		t.Fatalf("deriver calls=%d, want crash-window redo", fixture.chatDeriver.calls())
	}

	reopenedCheckpoints, _ := NewWorkspaceCheckpointStore(fixture.ws)
	reopened := fixture.makeProcessor(t, reopenedCheckpoints, fixture.indexers)
	if err := reopened.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	if fixture.chatDeriver.calls() != 2 {
		t.Fatalf("reopen repeated completed derivation, calls=%d", fixture.chatDeriver.calls())
	}
}

func TestSummaryBranchRetriesWithoutReplayingFactOrDuplicatingRecords(t *testing.T) {
	fixture := newFixture(t)
	fixture.putTurn(t, scopeA, "conversation", "turn", "stable summary")
	failing := &failBranchCheckpoint{CheckpointStore: fixture.checkpoints, branch: summaryBranch}
	fixture.processor = fixture.makeProcessor(t, failing, fixture.indexers)

	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err == nil {
		t.Fatal("summary checkpoint failure was not surfaced")
	}
	if fixture.chatDeriver.calls() != 1 {
		t.Fatalf("fact derivation calls=%d", fixture.chatDeriver.calls())
	}
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	if fixture.chatDeriver.calls() != 1 {
		t.Fatalf("summary retry replayed fact derivation, calls=%d", fixture.chatDeriver.calls())
	}
	records, err := fixture.summaries.ListActive(context.Background(), scopeA, "conversation", summaryview.ListOptions{})
	if err != nil || len(records) != 2 {
		t.Fatalf("summary records=%d err=%v", len(records), err)
	}
}

func TestSummaryManifestPublishesBeforeCheckpointCompletes(t *testing.T) {
	fixture := newFixture(t)
	fixture.putTurn(t, scopeA, "conversation", "turn", "stable summary")
	failing := &failManifestStore{Store: fixture.summaries}
	compactor, _ := summaryderive.New(summaryderive.DefaultConfig(), failing, nil)
	processor, err := NewProcessor(ProcessorConfig{
		Messages: fixture.messages, Documents: fixture.documents, Facts: fixture.facts,
		Summaries: failing, Compactor: compactor, DocumentViews: fixture.documentViews,
		ChatDAG: fixture.chatDAG, KnowledgeDAG: fixture.knowledgeDAG, Checkpoints: fixture.checkpoints,
		Projection: "memory", PolicyDigest: "policy-v1", Indexers: fixture.indexers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.ProcessScope(context.Background(), scopeA); err == nil {
		t.Fatal("manifest publish failure was not surfaced")
	}
	commits, _ := fixture.messages.ListCommits(context.Background(), scopeA, "conversation", msgsource.ListCommitOptions{})
	work := WorkIdentity{Kind: "message-commit", ID: commits[0].ID, PolicyDigest: "policy-v1"}
	checkpoint, ok, err := fixture.checkpoints.Load(context.Background(), scopeA, work, summaryBranch)
	if err != nil || !ok || checkpoint.Status == StatusComplete {
		t.Fatalf("checkpoint=%#v ok=%v err=%v", checkpoint, ok, err)
	}
	if _, ok, err := fixture.summaries.LoadActive(context.Background(), scopeA, "conversation"); err != nil || ok {
		t.Fatalf("manifest published before retry: ok=%v err=%v", ok, err)
	}
	if err := processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	checkpoint, ok, err = fixture.checkpoints.Load(context.Background(), scopeA, work, summaryBranch)
	if err != nil || !ok || checkpoint.Status != StatusComplete {
		t.Fatalf("checkpoint after retry=%#v ok=%v err=%v", checkpoint, ok, err)
	}
	if _, ok, err := fixture.summaries.LoadActive(context.Background(), scopeA, "conversation"); err != nil || !ok {
		t.Fatalf("manifest after retry: ok=%v err=%v", ok, err)
	}
}

func TestWorkerConsecutiveMessageCommitsReuseSummaryPrefix(t *testing.T) {
	fixture := newFixture(t)
	fixture.putTurn(t, scopeA, "conversation", "turn-1", "first")
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	first, err := fixture.summaries.ListActive(context.Background(), scopeA, "conversation", summaryview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	firstIDs := make(map[string]struct{}, len(first))
	for _, record := range first {
		firstIDs[record.ID] = struct{}{}
	}
	fixture.putTurn(t, scopeA, "conversation", "turn-2", "second")
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	second, err := fixture.summaries.ListActive(context.Background(), scopeA, "conversation", summaryview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reused := 0
	for _, record := range second {
		if _, ok := firstIDs[record.ID]; ok {
			reused++
		}
	}
	if reused == 0 {
		t.Fatalf("consecutive commits reused no summary records: first=%d second=%d", len(first), len(second))
	}
	manifest, ok, err := fixture.summaries.LoadActive(context.Background(), scopeA, "conversation")
	if err != nil || !ok || manifest.GenerationID == "" || len(manifest.RecordIDs) != len(second) {
		t.Fatalf("manifest=%#v ok=%v err=%v", manifest, ok, err)
	}
}

func TestProjectionLaneFailureDoesNotBlockOtherLanes(t *testing.T) {
	fixture := newFixture(t)
	fixture.putDocument(t, scopeA, "dataset", "document", "projection")
	fixture.indexers[0].Indexer.(*fakeIndexer).setFailures(1)
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err == nil {
		t.Fatal("projection failure not surfaced")
	}
	if fixture.indexers[1].Indexer.(*fakeIndexer).calls() != 1 ||
		fixture.indexers[2].Indexer.(*fakeIndexer).calls() != 1 {
		t.Fatal("healthy projection lanes did not continue")
	}
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	if fixture.indexers[0].Indexer.(*fakeIndexer).calls() != 2 {
		t.Fatal("failed projection lane was not retried")
	}
	if fixture.indexers[1].Indexer.(*fakeIndexer).calls() != 1 ||
		fixture.indexers[2].Indexer.(*fakeIndexer).calls() != 1 {
		t.Fatal("completed projection lanes were rebuilt on retry")
	}
}

func TestCheckpointRejectsCorruptionAndUnknownSchema(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store, _ := NewWorkspaceCheckpointStore(ws)
	work := WorkIdentity{Kind: "message-commit", ID: "work", PolicyDigest: "policy-v1"}
	for _, data := range [][]byte{
		[]byte(`{"schema_version":`),
		[]byte(`{"schema_version":99}`),
	} {
		ws.MustWrite(store.checkpointPath(scopeA, work, chatBranch), data)
		if _, _, err := store.Load(ctx, scopeA, work, chatBranch); err == nil {
			t.Fatal("corrupt checkpoint was accepted")
		}
	}
}

func TestRunnerImmediateScanCancelAndIdempotentClose(t *testing.T) {
	fixture := newFixture(t)
	fixture.putDocument(t, scopeA, "dataset", "document", "runner")
	signal := make(chan struct{}, 1)
	for _, lane := range fixture.indexers {
		lane.Indexer.(*fakeIndexer).signal = signal
	}
	runner, err := NewRunner(RunnerConfig{
		Processor: fixture.processor, Catalog: fixture.catalog,
		Scopes: []sdkmemory.Scope{scopeA}, Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("initial scan did not run immediately")
	}
	cancel()
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerDiscoversCatalogScopesAddedAfterConstruction(t *testing.T) {
	fixture := newFixture(t)
	dynamic := sdkmemory.Scope{RuntimeID: "runtime", UserID: "dynamic", AgentID: "agent"}
	runner, err := NewRunner(RunnerConfig{
		Processor: fixture.processor, Catalog: fixture.catalog, Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.putDocument(t, dynamic, "dataset", "document", "dynamic")
	if err := fixture.catalog.Register(context.Background(), dynamic); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	chunks, err := fixture.documentViews.List(context.Background(), dynamic, "dataset", "document", docview.ListOptions{})
	if err != nil || len(leafChunks(chunks)) != 1 {
		t.Fatalf("dynamic scope records=%d err=%v", len(chunks), err)
	}
}

func TestProcessorConsumesEveryDocumentRevisionAndTombstone(t *testing.T) {
	fixture := newFixture(t)
	fixture.putDocument(t, scopeA, "dataset", "document", "version one")
	fixture.putDocument(t, scopeA, "dataset", "document", "version two")
	events, err := fixture.documents.ListEvents(context.Background(), scopeA, docsource.ListEventOptions{})
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	chunks, err := fixture.documentViews.List(context.Background(), scopeA, "dataset", "document", docview.ListOptions{})
	leaf := leafChunks(chunks)
	if err != nil || len(leaf) != 1 || leaf[0].DocumentVersion != 2 ||
		leaf[0].Content.Text() != "version two" {
		t.Fatalf("latest chunks=%#v err=%v", chunks, err)
	}
	for _, event := range events {
		work := WorkIdentity{Kind: "document-event", ID: event.ID, PolicyDigest: "policy-v1"}
		checkpoint, ok, err := fixture.checkpoints.Load(context.Background(), scopeA, work, knowledgeBranch)
		if err != nil || !ok || checkpoint.Status != StatusComplete {
			t.Fatalf("checkpoint(%s)=%#v ok=%v err=%v", event.ID, checkpoint, ok, err)
		}
	}

	if err := fixture.documents.Delete(context.Background(), scopeA, "dataset", "document"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	chunks, err = fixture.documentViews.List(context.Background(), scopeA, "dataset", "document", docview.ListOptions{})
	if err != nil || len(chunks) != 0 {
		t.Fatalf("tombstoned chunks=%#v err=%v", chunks, err)
	}
	if _, ok, err := fixture.documents.Get(context.Background(), scopeA, "dataset", "document"); err != nil || ok {
		t.Fatalf("tombstoned current document ok=%v err=%v", ok, err)
	}
}

func TestCheckpointPolicyDigestChangeReprocessesWork(t *testing.T) {
	fixture := newFixture(t)
	fixture.putTurn(t, scopeA, "conversation", "turn", "policy")
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	if fixture.chatDeriver.calls() != 1 {
		t.Fatalf("initial calls=%d", fixture.chatDeriver.calls())
	}
	changed := fixture.makeProcessorWithPolicy(t, fixture.checkpoints, fixture.indexers, "policy-v2")
	if err := changed.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	if fixture.chatDeriver.calls() != 2 {
		t.Fatalf("policy change reused old checkpoint, calls=%d", fixture.chatDeriver.calls())
	}
}

func TestProcessorUsesDurableSourceWatermarks(t *testing.T) {
	fixture := newFixture(t)
	for index := 0; index < 200; index++ {
		fixture.putTurn(t, scopeA, "conversation", fmt.Sprint(index), fmt.Sprint(index))
	}
	for index := 0; index < 3; index++ {
		fixture.putDocument(t, scopeA, "dataset", fmt.Sprintf("document-%03d", index), fmt.Sprint(index))
	}
	messages := &countingMessageSource{Store: fixture.messages}
	documents := &countingDocumentSource{Store: fixture.documents}
	processor := fixture.makeLeanProcessorWithSources(t, messages, documents, fixture.checkpoints, "policy-v1")
	if err := processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	messages.reset()
	documents.reset()
	if err := processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	if calls, commits := messages.snapshot(); calls > 2 || commits != 0 {
		t.Fatalf("idle message polling calls=%d commits=%d", calls, commits)
	}
	if calls, events := documents.snapshot(); calls > 1 || events != 0 {
		t.Fatalf("idle document polling calls=%d events=%d", calls, events)
	}
}

func TestProcessorWatermarkFailureRetriesOnlyFailedCursor(t *testing.T) {
	fixture := newFixture(t)
	fixture.putTurn(t, scopeA, "conversation", "one", "one")
	fixture.putTurn(t, scopeA, "conversation", "two", "two")
	failing := &failWatermarkCheckpoint{CheckpointStore: fixture.checkpoints}
	processor := fixture.makeProcessorWithSources(t, fixture.messages, fixture.documents, failing, fixture.indexers, "policy-v1")
	if err := processor.ProcessScope(context.Background(), scopeA); err == nil {
		t.Fatal("watermark failure was not surfaced")
	}
	if err := processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	if fixture.chatDeriver.calls() != 2 {
		t.Fatalf("logical work repeated after watermark failure: calls=%d", fixture.chatDeriver.calls())
	}
}

func TestProcessorDoesNotCrossTenantScope(t *testing.T) {
	fixture := newFixture(t)
	fixture.putTurn(t, scopeA, "conversation", "a", "alice")
	fixture.putTurn(t, scopeB, "conversation", "b", "bob")
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	alice, _ := fixture.facts.List(context.Background(), scopeA, "conversation", factview.ListOptions{})
	bob, _ := fixture.facts.List(context.Background(), scopeB, "conversation", factview.ListOptions{})
	if len(alice) != 1 || len(bob) != 0 {
		t.Fatalf("tenant results alice=%d bob=%d", len(alice), len(bob))
	}
}

func TestProcessorDoesNotCrossAgentScope(t *testing.T) {
	fixture := newFixture(t)
	agentA := sdkmemory.Scope{RuntimeID: "runtime", UserID: "shared", AgentID: "agent-a"}
	agentB := sdkmemory.Scope{RuntimeID: "runtime", UserID: "shared", AgentID: "agent-b"}
	fixture.putTurn(t, agentA, "conversation", "a", "agent a")
	fixture.putTurn(t, agentB, "conversation", "b", "agent b")
	fixture.putDocument(t, agentA, "dataset", "document", "document a")
	fixture.putDocument(t, agentB, "dataset", "document", "document b")
	if err := fixture.processor.ProcessScope(context.Background(), agentA); err != nil {
		t.Fatal(err)
	}
	factsA, _ := fixture.facts.List(context.Background(), agentA, "conversation", factview.ListOptions{})
	factsB, _ := fixture.facts.List(context.Background(), agentB, "conversation", factview.ListOptions{})
	chunksA, _ := fixture.documentViews.List(context.Background(), agentA, "dataset", "document", docview.ListOptions{})
	chunksB, _ := fixture.documentViews.List(context.Background(), agentB, "dataset", "document", docview.ListOptions{})
	if len(factsA) != 1 || len(factsB) != 0 || len(leafChunks(chunksA)) != 1 || len(chunksB) != 0 {
		t.Fatalf("agent results facts=(%d,%d) chunks=(%d,%d)", len(factsA), len(factsB), len(chunksA), len(chunksB))
	}
}

func TestProcessorEmptyAgentScopeOverridesForgedMetadata(t *testing.T) {
	fixture := newFixture(t)
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "shared"}
	forged := sdkmemory.Scope{RuntimeID: "runtime", UserID: "shared", AgentID: "victim"}
	metadata := sdkmemory.Metadata{"agent_id": forged.AgentID}
	addScopeMetadata(metadata, scope)
	if metadata["agent_id"] != "" {
		t.Fatalf("empty agent scope retained forged partition metadata: %#v", metadata)
	}
	_, err := fixture.facts.Add(context.Background(), factview.AddRequest{
		ID: "victim-fact", Scope: forged, ConversationID: "conversation",
		Content:  sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "victim secret"}}},
		Entities: []string{"memory"}, EventTime: time.Now(),
		Provenance: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "victim-message"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.messages.Commit(context.Background(), msgsource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "forged-agent",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "belongs to the unscoped agent")},
		Metadata: sdkmemory.Metadata{"agent_id": forged.AgentID},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := fixture.processor.ProcessScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	unscoped, _ := fixture.facts.List(context.Background(), scope, "conversation", factview.ListOptions{})
	victim, _ := fixture.facts.List(context.Background(), forged, "conversation", factview.ListOptions{})
	if len(unscoped) != 1 || len(victim) != 1 {
		t.Fatalf("cross-agent metadata escape: unscoped=%d victim=%d", len(unscoped), len(victim))
	}
	if len(unscoped[0].LinkedMemoryIDs) != 0 {
		t.Fatalf("forged metadata linked victim partition: %v", unscoped[0].LinkedMemoryIDs)
	}
}

func leafChunks(records []docview.Chunk) []docview.Chunk {
	result := make([]docview.Chunk, 0, len(records))
	for _, record := range records {
		if record.Kind == docview.KindChunk {
			result = append(result, record)
		}
	}
	return result
}

func TestFactEntitiesFeedProjectionFromTypedState(t *testing.T) {
	fixture := newFixture(t)
	fixture.putTurn(t, scopeA, "conversation", "turn", "entity memory")
	if err := fixture.processor.ProcessScope(context.Background(), scopeA); err != nil {
		t.Fatal(err)
	}
	artifacts, err := fixture.processor.collectArtifacts(context.Background(), scopeA)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if artifact.Kind == chat.KindFact {
			if artifact.Metadata["entities"] != `["memory"]` {
				t.Fatalf("fact entity feed = %#v", artifact.Metadata)
			}
			return
		}
	}
	t.Fatal("fact projection artifact not found")
}

type fixture struct {
	t             *testing.T
	ws            *workspace.MemWorkspace
	messages      *msgsource.WorkspaceStore
	documents     *docsource.WorkspaceStore
	facts         *factview.WorkspaceStore
	summaries     *summaryview.WorkspaceStore
	compactor     *summaryderive.Compactor
	documentViews *docview.WorkspaceStore
	checkpoints   *WorkspaceCheckpointStore
	catalog       *sources.WorkspaceScopeCatalog
	chatDeriver   *fakeFactDeriver
	chatDAG       *derive.DAG
	knowledgeDAG  *derive.DAG
	indexers      []ProjectionIndexer
	processor     *Processor
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	messages, _ := msgsource.NewWorkspaceStore(ws)
	documents, _ := docsource.NewWorkspaceStore(ws)
	facts, _ := factview.NewWorkspaceStore(ws)
	summaries, _ := summaryview.NewWorkspaceStore(ws)
	compactor, _ := summaryderive.New(summaryderive.DefaultConfig(), summaries, nil)
	documentViews, _ := docview.NewWorkspaceStore(ws)
	checkpoints, _ := NewWorkspaceCheckpointStore(ws)
	catalog, _ := sources.NewWorkspaceScopeCatalog(ws)
	factDeriver := &fakeFactDeriver{}
	chatDAG := buildDAG(t, "facts", factDeriver)
	chunker, _ := knowledge.NewChunker(knowledge.ChunkerConfig{MaxRunes: 100})
	knowledgeDAG := buildDAG(t, "chunks", chunker)
	indexers := []ProjectionIndexer{
		{Name: "bm25", Indexer: &fakeIndexer{}},
		{Name: "vector", Indexer: &fakeIndexer{}},
		{Name: "entity", Indexer: &fakeIndexer{}},
	}
	value := &fixture{
		t: t, ws: ws, messages: messages, documents: documents, facts: facts,
		summaries: summaries, compactor: compactor,
		documentViews: documentViews, checkpoints: checkpoints, catalog: catalog, chatDeriver: factDeriver,
		chatDAG: chatDAG, knowledgeDAG: knowledgeDAG, indexers: indexers,
	}
	value.processor = value.makeProcessor(t, checkpoints, indexers)
	return value
}

func (fixture *fixture) makeProcessor(t *testing.T, checkpoints CheckpointStore, indexers []ProjectionIndexer) *Processor {
	return fixture.makeProcessorWithPolicy(t, checkpoints, indexers, "policy-v1")
}

func (fixture *fixture) makeProcessorWithPolicy(t *testing.T, checkpoints CheckpointStore, indexers []ProjectionIndexer, policyDigest string) *Processor {
	return fixture.makeProcessorWithSources(t, fixture.messages, fixture.documents, checkpoints, indexers, policyDigest)
}

func (fixture *fixture) makeProcessorWithSources(
	t *testing.T,
	messages msgsource.Store,
	documents docsource.Store,
	checkpoints CheckpointStore,
	indexers []ProjectionIndexer,
	policyDigest string,
) *Processor {
	t.Helper()
	processor, err := NewProcessor(ProcessorConfig{
		Messages: messages, Documents: documents, Facts: fixture.facts,
		Summaries: fixture.summaries, Compactor: fixture.compactor,
		DocumentViews: fixture.documentViews, ChatDAG: fixture.chatDAG,
		KnowledgeDAG: fixture.knowledgeDAG, Checkpoints: checkpoints,
		Projection: "memory", PolicyDigest: policyDigest, Indexers: indexers,
	})
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func (fixture *fixture) makeLeanProcessorWithSources(
	t *testing.T,
	messages msgsource.Store,
	documents docsource.Store,
	checkpoints CheckpointStore,
	policyDigest string,
) *Processor {
	t.Helper()
	processor, err := NewProcessor(ProcessorConfig{
		Messages: messages, Documents: documents, Facts: fixture.facts,
		DocumentViews: fixture.documentViews, ChatDAG: fixture.chatDAG,
		KnowledgeDAG: fixture.knowledgeDAG, Checkpoints: checkpoints,
		Projection: "memory", PolicyDigest: policyDigest, Indexers: fixture.indexers,
	})
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func (fixture *fixture) putTurn(t *testing.T, scope sdkmemory.Scope, conversation, key, text string) {
	t.Helper()
	_, err := fixture.messages.Commit(context.Background(), msgsource.AppendRequest{
		Scope: scope, ConversationID: conversation, IdempotencyKey: key,
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, text)},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (fixture *fixture) putDocument(t *testing.T, scope sdkmemory.Scope, dataset, documentID, text string) {
	t.Helper()
	_, err := fixture.documents.Put(context.Background(), docsource.PutRequest{
		Scope: scope, DatasetID: dataset, DocumentID: documentID, IdempotencyKey: text,
		Content:    sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}},
		Provenance: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceExternal, ID: "source"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func buildDAG(t *testing.T, name string, deriver component.Deriver) *derive.DAG {
	t.Helper()
	registry := component.NewRegistry()
	ports := component.Ports{
		Inputs:  []component.ArtifactKind{chat.KindRawMessage},
		Outputs: []component.ArtifactKind{chat.KindFact},
	}
	sourceKinds := []component.ArtifactKind{chat.KindRawMessage}
	if name == "chunks" {
		ports = component.Ports{
			Inputs: []component.ArtifactKind{knowledge.KindDocument},
			Outputs: []component.ArtifactKind{
				knowledge.KindResource, knowledge.KindSection, knowledge.KindDocumentChunk, knowledge.KindSummary,
			},
		}
		sourceKinds = []component.ArtifactKind{knowledge.KindDocument}
	}
	if err := component.RegisterTypedDeriver(
		registry,
		name,
		"test",
		ports,
		func(struct{}) (component.Deriver, error) { return deriver, nil },
	); err != nil {
		t.Fatal(err)
	}
	dag, err := derive.Build(registry, derive.Spec{
		SourceKinds: sourceKinds,
		Nodes: []derive.NodeSpec{{
			ID: name, Deriver: component.Spec{Name: name},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dag
}

type fakeFactDeriver struct {
	mu   sync.Mutex
	fail bool
	n    int
}

func (deriver *fakeFactDeriver) Derive(ctx context.Context, input component.Artifact) ([]component.Artifact, error) {
	deriver.mu.Lock()
	defer deriver.mu.Unlock()
	deriver.n++
	if deriver.fail {
		return nil, errors.New("fact derivation failed")
	}
	return []component.Artifact{{
		Kind: chat.KindFact, ID: "fact-" + input.ID, Content: input.Content.Clone(),
		Sources:  append([]sdkmemory.SourceRef(nil), input.Sources...),
		Metadata: sdkmemory.Metadata{"entities": "memory"},
	}}, nil
}

func (deriver *fakeFactDeriver) setFail(value bool) {
	deriver.mu.Lock()
	deriver.fail = value
	deriver.mu.Unlock()
}

func (deriver *fakeFactDeriver) calls() int {
	deriver.mu.Lock()
	defer deriver.mu.Unlock()
	return deriver.n
}

type fakeIndexer struct {
	mu       sync.Mutex
	n        int
	failures int
	signal   chan struct{}
}

func (indexer *fakeIndexer) Rebuild(ctx context.Context, request component.ProjectionRequest) error {
	indexer.mu.Lock()
	defer indexer.mu.Unlock()
	indexer.n++
	if indexer.signal != nil {
		select {
		case indexer.signal <- struct{}{}:
		default:
		}
	}
	if indexer.failures > 0 {
		indexer.failures--
		return errors.New("projection failed")
	}
	return nil
}

func (indexer *fakeIndexer) setFailures(value int) {
	indexer.mu.Lock()
	indexer.failures = value
	indexer.mu.Unlock()
}

func (indexer *fakeIndexer) calls() int {
	indexer.mu.Lock()
	defer indexer.mu.Unlock()
	return indexer.n
}

type failCompleteCheckpoint struct {
	CheckpointStore
	mu     sync.Mutex
	failed bool
}

type failManifestStore struct {
	summaryview.Store
	mu     sync.Mutex
	failed bool
}

func (store *failManifestStore) PublishActive(ctx context.Context, manifest summaryview.Manifest) error {
	store.mu.Lock()
	if !store.failed {
		store.failed = true
		store.mu.Unlock()
		return errors.New("manifest publish failed")
	}
	store.mu.Unlock()
	return store.Store.PublishActive(ctx, manifest)
}

type countingMessageSource struct {
	msgsource.Store
	mu      sync.Mutex
	calls   int
	commits int
}

func (source *countingMessageSource) ListCommits(
	ctx context.Context,
	scope sdkmemory.Scope,
	conversationID string,
	options msgsource.ListCommitOptions,
) ([]msgsource.Commit, error) {
	values, err := source.Store.ListCommits(ctx, scope, conversationID, options)
	source.mu.Lock()
	source.calls++
	source.commits += len(values)
	source.mu.Unlock()
	return values, err
}

func (source *countingMessageSource) reset() {
	source.mu.Lock()
	source.calls = 0
	source.commits = 0
	source.mu.Unlock()
}

func (source *countingMessageSource) snapshot() (int, int) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls, source.commits
}

type countingDocumentSource struct {
	docsource.Store
	mu     sync.Mutex
	calls  int
	events int
}

func (source *countingDocumentSource) ListEvents(
	ctx context.Context,
	scope sdkmemory.Scope,
	options docsource.ListEventOptions,
) ([]docsource.Event, error) {
	values, err := source.Store.ListEvents(ctx, scope, options)
	source.mu.Lock()
	source.calls++
	source.events += len(values)
	source.mu.Unlock()
	return values, err
}

func (source *countingDocumentSource) reset() {
	source.mu.Lock()
	source.calls = 0
	source.events = 0
	source.mu.Unlock()
}

func (source *countingDocumentSource) snapshot() (int, int) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls, source.events
}

type failWatermarkCheckpoint struct {
	CheckpointStore
	mu     sync.Mutex
	failed bool
}

func (store *failWatermarkCheckpoint) SaveWatermark(ctx context.Context, watermark SourceWatermark) error {
	store.mu.Lock()
	if !store.failed && watermark.Cursor > 0 {
		store.failed = true
		store.mu.Unlock()
		return errors.New("watermark publish failed")
	}
	store.mu.Unlock()
	return store.CheckpointStore.SaveWatermark(ctx, watermark)
}

type failBranchCheckpoint struct {
	CheckpointStore
	mu     sync.Mutex
	branch string
	failed bool
}

func (store *failBranchCheckpoint) Save(ctx context.Context, checkpoint Checkpoint) error {
	store.mu.Lock()
	if !store.failed && checkpoint.Branch == store.branch && checkpoint.Status == StatusComplete {
		store.failed = true
		store.mu.Unlock()
		return errors.New("checkpoint publish failed")
	}
	store.mu.Unlock()
	return store.CheckpointStore.Save(ctx, checkpoint)
}

func (store *failCompleteCheckpoint) Save(ctx context.Context, checkpoint Checkpoint) error {
	store.mu.Lock()
	if !store.failed && checkpoint.Branch == chatBranch && checkpoint.Status == StatusComplete {
		store.failed = true
		store.mu.Unlock()
		return errors.New("checkpoint publish failed")
	}
	store.mu.Unlock()
	return store.CheckpointStore.Save(ctx, checkpoint)
}
