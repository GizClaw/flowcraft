package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	summaryderive "github.com/GizClaw/flowcraft/backends/memory/derive/summary"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	"github.com/GizClaw/flowcraft/backends/memory/lines/knowledge"
	docsource "github.com/GizClaw/flowcraft/backends/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	docview "github.com/GizClaw/flowcraft/backends/memory/views/document"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

const (
	streamKindMessages  = "message"
	streamKindDocuments = "document-events"
	documentStreamID    = "scope"
	defaultPageSize     = 64
	// maxDeriveConcurrency caps how many conversations one scope derives at
	// once; beyond a handful the bottleneck is the model provider, and more
	// goroutines only add rate-limit pressure.
	maxDeriveConcurrency = 8
)

// ProjectionIndexer names one projection lane fed by the worker.
type ProjectionIndexer struct {
	Name    string
	Indexer component.DeltaIndexer
}

// MessageReader is the narrow canonical message source the worker needs.
// *msgsource.MessageStore satisfies it structurally.
type MessageReader interface {
	ListConversations(context.Context, corememory.Scope) ([]string, error)
	ListCommits(context.Context, corememory.Scope, string, msgsource.ListCommitOptions) ([]msgsource.Commit, error)
	List(context.Context, corememory.Scope, string, msgsource.ListOptions) ([]msgsource.Record, error)
}

// DocumentReader is the narrow canonical document source the worker needs.
// *docsource.DocumentStore satisfies it structurally.
type DocumentReader interface {
	ListEvents(context.Context, corememory.Scope, docsource.ListEventOptions) ([]docsource.Event, error)
}

// Config wires the chat derivation pipeline.
type Config struct {
	Messages         MessageReader
	Documents        DocumentReader
	DocumentViews    *docview.DocumentViewStore
	Facts            *factview.FactStore
	Deriver          component.Deriver
	KnowledgeDeriver component.Deriver
	Compactor        *summaryderive.Compactor
	Indexers         []ProjectionIndexer
	Checkpoints      CheckpointStore
	Projection       string
	PolicyDigest     string
	PageSize         int
	// Concurrency bounds how many conversations of one scope are derived in
	// parallel. Zero or one keeps the sequential pass. Each conversation keeps
	// its own watermark and walks its commits in order, so what a conversation
	// derives is unchanged; only the interleaving across conversations differs,
	// and every artifact id is content-derived.
	Concurrency int
}

// PolicyDigest returns the derivation policy digest this processor was built
// with. Stored watermarks are keyed by it, so hosts can tell that a workspace
// was written under a different policy.
func (processor *Processor) PolicyDigest() string {
	if processor == nil {
		return ""
	}
	return processor.policyDigest
}

// Processor scans canonical messages and publishes facts plus projection
// deltas exactly once per policy-scoped watermark.
type Processor struct {
	messages      MessageReader
	documents     DocumentReader
	documentViews *docview.DocumentViewStore
	facts         *factview.FactStore
	deriver       component.Deriver
	knowledge     component.Deriver
	compactor     *summaryderive.Compactor
	indexers      []ProjectionIndexer
	checkpoints   CheckpointStore
	projection    string
	policyDigest  string
	pageSize      int
	concurrency   int

	statsMu sync.Mutex
	stats   Stats
}

// NewProcessor validates the pipeline wiring.
func NewProcessor(config Config) (*Processor, error) {
	if nilInterface(config.Messages) || config.Facts == nil || nilInterface(config.Checkpoints) {
		return nil, errors.New("memory worker: messages, facts, and checkpoints are required")
	}
	if (nilInterface(config.Documents)) != (config.DocumentViews == nil) {
		return nil, errors.New("memory worker: documents and document views must be configured together")
	}
	if config.KnowledgeDeriver != nil && nilInterface(config.Documents) {
		return nil, errors.New("memory worker: knowledge derivation requires the document source")
	}
	if strings.TrimSpace(config.Projection) == "" {
		return nil, errors.New("memory worker: projection name is required")
	}
	if strings.TrimSpace(config.PolicyDigest) == "" {
		return nil, errors.New("memory worker: policy digest is required")
	}
	if len(config.Indexers) == 0 {
		return nil, errors.New("memory worker: at least one projection indexer is required")
	}
	seen := make(map[string]struct{}, len(config.Indexers))
	indexers := make([]ProjectionIndexer, 0, len(config.Indexers))
	for index, lane := range config.Indexers {
		if strings.TrimSpace(lane.Name) == "" || nilInterface(lane.Indexer) {
			return nil, fmt.Errorf("memory worker: indexer %d name and implementation are required", index)
		}
		if _, duplicate := seen[lane.Name]; duplicate {
			return nil, fmt.Errorf("memory worker: duplicate projection indexer %q", lane.Name)
		}
		seen[lane.Name] = struct{}{}
		indexers = append(indexers, lane)
	}
	pageSize := config.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	concurrency := config.Concurrency
	if concurrency < 0 {
		return nil, errors.New("memory worker: concurrency must not be negative")
	}
	if concurrency > maxDeriveConcurrency {
		concurrency = maxDeriveConcurrency
	}
	return &Processor{
		messages: config.Messages, documents: config.Documents, documentViews: config.DocumentViews,
		facts: config.Facts, deriver: config.Deriver, knowledge: config.KnowledgeDeriver,
		compactor: config.Compactor,
		indexers:  indexers, checkpoints: config.Checkpoints,
		projection: config.Projection, policyDigest: config.PolicyDigest, pageSize: pageSize,
		concurrency: concurrency,
	}, nil
}

// ProcessScope scans every conversation of one hard scope.
func (processor *Processor) ProcessScope(ctx context.Context, scope corememory.Scope) (err error) {
	defer func() {
		if err != nil {
			processor.RecordError(err)
		}
	}()
	if processor == nil {
		return errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	conversations, err := processor.messages.ListConversations(ctx, scope)
	if err != nil {
		return fmt.Errorf("memory worker: list conversations: %w", err)
	}
	var failures []error
	for _, failure := range processor.processConversations(ctx, scope, conversations) {
		if failure != nil {
			failures = append(failures, failure)
		}
	}
	if processor.knowledge != nil {
		if err := processor.processDocumentStream(ctx, scope); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// processConversations derives the conversations of one scope, optionally in
// parallel, and returns one failure per conversation (nil when it succeeded),
// ordered like the input. Concurrency only changes which conversation a worker
// picks next: each conversation is owned by exactly one goroutine and walks its
// own watermark in commit order, so the derived artifacts of a conversation are
// identical to a sequential pass.
func (processor *Processor) processConversations(ctx context.Context, scope corememory.Scope, conversations []string) []error {
	if len(conversations) == 0 {
		return nil
	}
	failures := make([]error, len(conversations))
	work := func(index int) {
		conversationID := conversations[index]
		if _, err := processor.ProcessConversation(ctx, scope, conversationID); err != nil {
			// One failing conversation must not block the derivation of
			// the others: its watermark stays put and it is retried on the
			// next pass, while healthy conversations still make progress.
			failures[index] = fmt.Errorf("memory worker: conversation %q: %w", conversationID, err)
		}
	}
	concurrency := processor.concurrency
	if concurrency > len(conversations) {
		concurrency = len(conversations)
	}
	if concurrency <= 1 {
		for index := range conversations {
			if err := ctx.Err(); err != nil {
				failures[index] = err
				break
			}
			work(index)
		}
		return failures
	}
	var (
		next  atomic.Int64
		group sync.WaitGroup
	)
	for worker := 0; worker < concurrency; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= len(conversations) {
					return
				}
				if ctx.Err() != nil {
					failures[index] = ctx.Err()
					return
				}
				work(index)
			}
		}()
	}
	group.Wait()
	return failures
}

// ProcessConversation scans commits after the stored watermark and returns
// the number of commits processed.
func (processor *Processor) ProcessConversation(ctx context.Context, scope corememory.Scope, conversationID string) (int, error) {
	if processor == nil {
		return 0, errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return 0, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(conversationID) == "" {
		return 0, errors.New("memory worker: conversation id is required")
	}
	cursor := uint64(0)
	if watermark, found, err := processor.checkpoints.LoadWatermark(
		ctx, scope, streamKindMessages, conversationID, processor.policyDigest,
	); err != nil {
		return 0, err
	} else if found {
		cursor = watermark.Cursor
	}
	processed := 0
	for {
		commits, err := processor.messages.ListCommits(ctx, scope, conversationID, msgsource.ListCommitOptions{
			AfterVersion: cursor, Limit: processor.pageSize,
		})
		if err != nil {
			return processed, fmt.Errorf("memory worker: list commits: %w", err)
		}
		if len(commits) == 0 {
			return processed, nil
		}
		for _, commit := range commits {
			artifacts, err := processor.processCommit(ctx, commit)
			if err != nil {
				return processed, err
			}
			for _, lane := range processor.indexers {
				if err := lane.Indexer.ApplyDelta(ctx, component.ProjectionDelta{
					Scope: commit.Scope, Projection: processor.projection,
					Upserts: artifacts, SourceRevision: commit.ID,
				}); err != nil {
					return processed, fmt.Errorf("memory worker: index %q: %w", lane.Name, err)
				}
				processor.bump(func(stats *Stats) { stats.IndexDeltasApplied++ })
			}
			cursor = commit.Version
			if err := processor.checkpoints.SaveWatermark(ctx, SourceWatermark{
				Scope: commit.Scope, StreamKind: streamKindMessages, StreamID: conversationID,
				PolicyDigest: processor.policyDigest, Cursor: cursor,
			}); err != nil {
				return processed, err
			}
			processed++
			processor.bump(func(stats *Stats) { stats.CommitsProcessed++ })
		}
		if len(commits) < processor.pageSize {
			return processed, nil
		}
	}
}

// processDocumentStream scans canonical document events after the stored
// scope-wide outbox cursor.
func (processor *Processor) processDocumentStream(ctx context.Context, scope corememory.Scope) error {
	cursor := uint64(0)
	if watermark, found, err := processor.checkpoints.LoadWatermark(
		ctx, scope, streamKindDocuments, documentStreamID, processor.policyDigest,
	); err != nil {
		return err
	} else if found {
		cursor = watermark.Cursor
	}
	for {
		events, err := processor.documents.ListEvents(ctx, scope, docsource.ListEventOptions{
			AfterOutboxSeq: cursor, Limit: processor.pageSize,
		})
		if err != nil {
			return fmt.Errorf("memory worker: list document events: %w", err)
		}
		if len(events) == 0 {
			return nil
		}
		for _, event := range events {
			if err := processor.processKnowledge(ctx, event); err != nil {
				return err
			}
			cursor = event.OutboxSeq
			if err := processor.checkpoints.SaveWatermark(ctx, SourceWatermark{
				Scope: event.Scope, StreamKind: streamKindDocuments, StreamID: documentStreamID,
				PolicyDigest: processor.policyDigest, Cursor: cursor,
			}); err != nil {
				return err
			}
		}
		if len(events) < processor.pageSize {
			return nil
		}
	}
}

// processKnowledge derives the document hierarchy for one canonical revision
// and reconciles its projection entries.
func (processor *Processor) processKnowledge(ctx context.Context, event docsource.Event) error {
	switch event.Operation {
	case docsource.OperationTombstone:
		if _, err := processor.documentViews.ReplaceDocument(ctx, docview.ReplaceRequest{
			Scope: event.Scope, DatasetID: event.DatasetID, DocumentID: event.DocumentID,
			DocumentVersion: event.Version, Chunks: []docview.Chunk{},
		}); err != nil {
			return fmt.Errorf("memory worker: tombstone document %q: %w", event.ID, err)
		}
		processor.bump(func(stats *Stats) { stats.DocumentsProcessed++ })
		return processor.applyKnowledgeDelta(ctx, event, nil, nil)
	case docsource.OperationPut:
		if event.Document == nil {
			return fmt.Errorf("memory worker: put event %q has no document", event.ID)
		}
		document := *event.Document
		derived, err := processor.knowledge.Derive(ctx, documentSource(document))
		if err != nil {
			return fmt.Errorf("memory worker: derive document %q: %w", event.ID, err)
		}
		chunks := make([]docview.Chunk, 0, len(derived))
		for _, artifact := range derived {
			recordKind, _, ok := knowledgeRecordKinds(artifact.Kind)
			if !ok {
				continue
			}
			level, _ := strconv.Atoi(artifact.Metadata["level"])
			ordinal, _ := strconv.ParseUint(artifact.Metadata["ordinal"], 10, 64)
			chunks = append(chunks, docview.Chunk{
				ID: artifact.ID, Kind: recordKind, Level: level,
				ParentID: artifact.Metadata["parent_id"], Title: artifact.Metadata["title"],
				Scope: document.Scope, DatasetID: document.DatasetID, DocumentID: document.DocumentID,
				DocumentVersion: document.Version, Ordinal: ordinal,
				Content:            artifact.Content.Clone(),
				Provenance:         append([]corememory.SourceRef(nil), artifact.Sources...),
				SourceDigest:       artifact.Metadata["source_digest"],
				TransformSignature: artifact.Metadata["transform_signature"],
				Metadata:           artifact.Metadata.Clone(),
			})
		}
		if _, err := processor.documentViews.ReplaceDocument(ctx, docview.ReplaceRequest{
			Scope: event.Scope, DatasetID: event.DatasetID, DocumentID: event.DocumentID,
			DocumentVersion: event.Version, Chunks: chunks,
		}); err != nil {
			return fmt.Errorf("memory worker: publish document view %q: %w", event.ID, err)
		}
		processor.bump(func(stats *Stats) {
			stats.DocumentsProcessed++
			stats.ChunksPublished += int64(len(chunks))
		})
		stored, err := processor.documentViews.List(
			ctx, event.Scope, event.DatasetID, event.DocumentID, docview.ListOptions{})
		if err != nil {
			return fmt.Errorf("memory worker: list document view %q: %w", event.ID, err)
		}
		artifacts, activeIDs := chunkArtifacts(event, stored)
		return processor.applyKnowledgeDelta(ctx, event, artifacts, activeIDs)
	default:
		return fmt.Errorf("memory worker: unsupported document operation %q", event.Operation)
	}
}

func (processor *Processor) applyKnowledgeDelta(
	ctx context.Context,
	event docsource.Event,
	artifacts []component.Artifact,
	activeIDs []string,
) error {
	delta := component.ProjectionDelta{
		Scope: event.Scope, Projection: processor.projection,
		Upserts:            artifacts,
		ReconcileDocuments: []component.DocumentAddress{{DatasetID: event.DatasetID, DocumentID: event.DocumentID}},
		ActiveIDs:          activeIDs,
		SourceRevision:     "document-event:" + event.ID,
		SourceDigest:       projectionSourceDigest(artifacts),
	}
	for _, lane := range processor.indexers {
		if err := lane.Indexer.ApplyDelta(ctx, delta); err != nil {
			return fmt.Errorf("memory worker: index %q: %w", lane.Name, err)
		}
		processor.bump(func(stats *Stats) { stats.IndexDeltasApplied++ })
	}
	return nil
}

func chunkArtifacts(event docsource.Event, chunks []docview.Chunk) ([]component.Artifact, []string) {
	artifacts := make([]component.Artifact, 0, len(chunks))
	activeIDs := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		artifactKind, contextKind := viewRecordKinds(chunk.Kind)
		metadata := addressMetadata(chunk.Metadata, contextKind, "", event.DatasetID, event.DocumentID, chunk.ID)
		addScopeMetadata(metadata, event.Scope)
		metadata["document_version"] = strconv.FormatUint(chunk.DocumentVersion, 10)
		metadata["ordinal"] = strconv.FormatUint(chunk.Ordinal, 10)
		artifact := component.Artifact{
			Kind:     artifactKind,
			ID:       projectionArtifactID("chunk", event.DatasetID, event.DocumentID, chunk.ID),
			Content:  chunk.Content.Clone(),
			Sources:  append([]corememory.SourceRef(nil), chunk.Provenance...),
			Metadata: metadata,
		}
		artifacts = append(artifacts, artifact)
		activeIDs = append(activeIDs, artifact.ID)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].ID < artifacts[j].ID })
	sort.Strings(activeIDs)
	return artifacts, activeIDs
}

func documentSource(document docsource.Document) component.Artifact {
	metadata := addressMetadata(
		document.Metadata, corememory.ContextDocumentChunk, "", document.DatasetID, document.DocumentID, document.DocumentID)
	addScopeMetadata(metadata, document.Scope)
	metadata["document_version"] = strconv.FormatUint(document.Version, 10)
	sources := []corememory.SourceRef{{
		Kind:     corememory.SourceDocument,
		ID:       document.DatasetID + "/" + document.DocumentID,
		Revision: strconv.FormatUint(document.Version, 10),
	}}
	sources = append(sources, document.Provenance...)
	return component.Artifact{
		Kind:    knowledge.KindDocument,
		ID:      projectionArtifactID("document", document.DatasetID, document.DocumentID, strconv.FormatUint(document.Version, 10)),
		Content: document.Content.Clone(), Sources: sources, Metadata: metadata,
	}
}

func knowledgeRecordKinds(kind component.ArtifactKind) (docview.RecordKind, corememory.ContextItemKind, bool) {
	switch kind {
	case knowledge.KindResource:
		return docview.KindResource, corememory.ContextDocumentResource, true
	case knowledge.KindSection:
		return docview.KindSection, corememory.ContextDocumentSection, true
	case knowledge.KindChunk:
		return docview.KindChunk, corememory.ContextDocumentChunk, true
	case knowledge.KindSummary:
		return docview.KindSummary, corememory.ContextDocumentSummary, true
	default:
		return "", "", false
	}
}

func viewRecordKinds(kind docview.RecordKind) (component.ArtifactKind, corememory.ContextItemKind) {
	switch kind {
	case docview.KindResource:
		return knowledge.KindResource, corememory.ContextDocumentResource
	case docview.KindSection:
		return knowledge.KindSection, corememory.ContextDocumentSection
	case docview.KindSummary:
		return knowledge.KindSummary, corememory.ContextDocumentSummary
	default:
		return knowledge.KindChunk, corememory.ContextDocumentChunk
	}
}

func projectionSourceDigest(artifacts []component.Artifact) string {
	hash := sha256.New()
	for _, artifact := range artifacts {
		_, _ = hash.Write([]byte(artifact.ID))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(artifact.Content.Text()))
		_, _ = hash.Write([]byte{0})
		for _, entity := range artifact.Entities {
			_, _ = hash.Write([]byte(entity))
			_, _ = hash.Write([]byte{0})
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// processCommit derives facts for one commit and returns the projection
// artifacts for its raw messages plus derived facts.
func (processor *Processor) processCommit(ctx context.Context, commit msgsource.Commit) ([]component.Artifact, error) {
	if len(commit.Records) == 0 {
		return nil, nil
	}
	artifacts := make([]component.Artifact, 0, len(commit.Records)+4)
	for _, record := range commit.Records {
		artifacts = append(artifacts, messageArtifact(commit, record))
	}
	if processor.deriver == nil {
		return artifacts, nil
	}
	derived, err := processor.deriver.Derive(ctx, chatSource(commit))
	if err != nil {
		return nil, fmt.Errorf("memory worker: derive commit %q: %w", commit.ID, err)
	}
	for _, artifact := range derived {
		if artifact.Kind != chat.KindFact {
			continue
		}
		fact, err := processor.facts.Add(ctx, factAddRequest(commit, artifact))
		if err != nil {
			return nil, fmt.Errorf("memory worker: publish fact %q: %w", artifact.ID, err)
		}
		processor.bump(func(stats *Stats) { stats.FactsPublished++ })
		artifacts = append(artifacts, factArtifact(fact))
	}
	if err := processor.compact(ctx, commit); err != nil {
		return nil, err
	}
	return artifacts, nil
}

// compact refreshes the summary branch after one commit. Compaction runs
// before the watermark advances so a failure replays derivation; fact
// publication is idempotent by canonical hash.
//
// The complete conversation window is compacted, never just the facts of the
// current commit: Compact replaces the active manifest, so a partial window
// would drop the summary records of every earlier commit. Records are
// content-addressed, so re-compacting the window reuses existing records and
// only derives the new tail groups.
func (processor *Processor) compact(ctx context.Context, commit msgsource.Commit) error {
	if processor.compactor == nil {
		return nil
	}
	values, err := processor.facts.List(ctx, commit.Scope, commit.ConversationID, factview.ListOptions{})
	if err != nil {
		return fmt.Errorf("memory worker: list facts for compaction: %w", err)
	}
	if len(values) == 0 {
		return nil
	}
	inputs := make([]summaryderive.Input, 0, len(values))
	for _, value := range values {
		inputs = append(inputs, summaryderive.InputFromFact(value))
	}
	if _, err := processor.compactor.Compact(ctx, summaryderive.CompactRequest{
		Scope: commit.Scope, ConversationID: commit.ConversationID,
		GenerationID: "message-commit:" + commit.ID, PolicySignature: processor.policyDigest,
		Inputs: inputs,
	}); err != nil {
		return fmt.Errorf("memory worker: compact summaries: %w", err)
	}
	return nil
}

func chatSource(commit msgsource.Commit) component.Artifact {
	var text strings.Builder
	sources := make([]corememory.SourceRef, 0, len(commit.Records))
	for _, record := range commit.Records {
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(string(record.Message.Role))
		text.WriteString(": ")
		text.WriteString(record.Message.Content.Text())
		sources = append(sources, messageSourceRef(record))
	}
	metadata := addressMetadata(commit.Records[0].Metadata, corememory.ContextRawMessage, commit.ConversationID, "", "", commit.Records[0].ID)
	addScopeMetadata(metadata, commit.Scope)
	metadata["commit_id"] = commit.ID
	metadata["commit_version"] = strconv.FormatUint(commit.Version, 10)
	metadata["event_time"] = commit.CreatedAt.UTC().Format(time.RFC3339Nano)
	return component.Artifact{
		Kind:    chat.KindRawMessage,
		ID:      commit.ID,
		Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text.String()}}},
		Sources: sources, Metadata: metadata,
	}
}

func messageArtifact(commit msgsource.Commit, record msgsource.Record) component.Artifact {
	metadata := addressMetadata(record.Metadata, corememory.ContextRawMessage, commit.ConversationID, "", "", record.ID)
	addScopeMetadata(metadata, commit.Scope)
	metadata["message_seq"] = strconv.FormatUint(record.Seq, 10)
	return component.Artifact{
		Kind: chat.KindRawMessage, ID: projectionArtifactID("message", commit.ConversationID, record.ID),
		Content:  record.Message.Content.Clone(),
		Sources:  []corememory.SourceRef{messageSourceRef(record)},
		Metadata: metadata,
	}
}

func factAddRequest(commit msgsource.Commit, artifact component.Artifact) factview.AddRequest {
	metadata := addressMetadata(artifact.Metadata, corememory.ContextFact, commit.ConversationID, "", "", artifact.ID)
	addScopeMetadata(metadata, commit.Scope)
	metadata["commit_version"] = strconv.FormatUint(commit.Version, 10)
	eventTime, _ := time.Parse(time.RFC3339Nano, artifact.Metadata["event_time"])
	if eventTime.IsZero() {
		// A deriver that could not date the fact leaves event_time unset; the
		// fact view requires one, and the commit's creation time is the only
		// timestamp we actually know. Dating it 1970 would make it look
		// simultaneous with every other undated fact and decay it to nothing.
		eventTime = commit.CreatedAt.UTC()
	}
	return factview.AddRequest{
		ID: artifact.ID, Scope: commit.Scope, ConversationID: commit.ConversationID,
		Content: artifact.Content, Provenance: artifact.Sources, Metadata: metadata,
		CanonicalHash: artifact.Metadata["canonical_hash"],
		Entities:      decodeMetadataStrings(artifact.Metadata["entities"]),
		Predicate:     artifact.Metadata["predicate"], TemporalDetail: artifact.Metadata["temporal_detail"],
		EventTime: eventTime, LinkedMemoryIDs: decodeMetadataStrings(artifact.Metadata["linked_memory_ids"]),
		SourceDigest:       artifact.Metadata["source_digest"],
		TransformSignature: artifact.Metadata["transform_signature"],
	}
}

func factArtifact(fact factview.Fact) component.Artifact {
	metadata := fact.Metadata.Clone()
	if metadata == nil {
		metadata = corememory.Metadata{}
	}
	metadata["context_kind"] = string(corememory.ContextFact)
	metadata["item_id"] = fact.ID
	if fact.ConversationID != "" {
		metadata["conversation_id"] = fact.ConversationID
	}
	metadata["runtime_id"] = fact.Scope.RuntimeID
	metadata["user_id"] = fact.Scope.UserID
	metadata["agent_id"] = fact.Scope.AgentID
	metadata["canonical_hash"] = fact.CanonicalHash
	if len(fact.Entities) > 0 {
		metadata["entities"] = encodeMetadataStrings(fact.Entities)
	}
	if !fact.EventTime.IsZero() {
		metadata["event_time"] = fact.EventTime.UTC().Format(time.RFC3339Nano)
	}
	return component.Artifact{
		Kind: chat.KindFact, ID: fact.ID,
		Content: fact.Content.Clone(), Entities: append([]string(nil), fact.Entities...),
		Sources:  append([]corememory.SourceRef(nil), fact.Provenance...),
		Metadata: metadata,
	}
}

func messageSourceRef(record msgsource.Record) corememory.SourceRef {
	return corememory.SourceRef{
		Kind: corememory.SourceMessage, ID: record.ConversationID + "/" + record.ID,
		Revision: strconv.FormatUint(record.Seq, 10),
	}
}

func projectionArtifactID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func addressMetadata(
	source corememory.Metadata,
	kind corememory.ContextItemKind,
	conversationID, datasetID, documentID, itemID string,
) corememory.Metadata {
	metadata := make(corememory.Metadata, len(source)+6)
	for key, value := range source {
		metadata[key] = value
	}
	metadata["context_kind"] = string(kind)
	metadata["item_id"] = itemID
	if conversationID != "" {
		metadata["conversation_id"] = conversationID
	}
	if datasetID != "" {
		metadata["dataset_id"] = datasetID
	}
	if documentID != "" {
		metadata["document_id"] = documentID
	}
	return metadata
}

func addScopeMetadata(metadata corememory.Metadata, scope corememory.Scope) {
	metadata["runtime_id"] = scope.RuntimeID
	metadata["user_id"] = scope.UserID
	metadata["agent_id"] = scope.AgentID
}

func decodeMetadataStrings(value string) []string {
	var result []string
	if json.Unmarshal([]byte(value), &result) != nil {
		result = strings.Split(value, ",")
	}
	return factview.NormalizeEntities(result)
}

func encodeMetadataStrings(values []string) string {
	data, _ := json.Marshal(values)
	return string(data)
}
