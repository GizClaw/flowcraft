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
	"time"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/derive"
	summaryderive "github.com/GizClaw/flowcraft/memory/derive/summary"
	"github.com/GizClaw/flowcraft/memory/lines/chat"
	"github.com/GizClaw/flowcraft/memory/lines/knowledge"
	docsource "github.com/GizClaw/flowcraft/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/memory/sources/message"
	docview "github.com/GizClaw/flowcraft/memory/views/document"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/memory/views/summary"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

const (
	chatBranch      = "chat-view"
	knowledgeBranch = "knowledge-view"
	summaryBranch   = "summary-compact"
	sourcePageSize  = 64
)

type ProjectionIndexer struct {
	Name    string
	Indexer component.Indexer
}

type ProcessorConfig struct {
	Messages      msgsource.Store
	Documents     docsource.Store
	Facts         factview.Store
	Summaries     summaryview.Store
	Compactor     *summaryderive.Compactor
	DocumentViews docview.Store
	ChatDAG       *derive.DAG
	KnowledgeDAG  *derive.DAG
	Checkpoints   CheckpointStore
	Projection    string
	PolicyDigest  string
	Indexers      []ProjectionIndexer
}

// Processor scans and processes exactly one explicitly supplied hard scope.
type Processor struct {
	messages      msgsource.Store
	documents     docsource.Store
	facts         factview.Store
	summaries     summaryview.Store
	compactor     *summaryderive.Compactor
	documentViews docview.Store
	chatDAG       *derive.DAG
	knowledgeDAG  *derive.DAG
	checkpoints   CheckpointStore
	projection    string
	policyDigest  string
	indexers      []ProjectionIndexer
}

func NewProcessor(config ProcessorConfig) (*Processor, error) {
	if nilInterface(config.Messages) || nilInterface(config.Documents) ||
		nilInterface(config.Facts) || nilInterface(config.DocumentViews) ||
		config.ChatDAG == nil || config.KnowledgeDAG == nil || nilInterface(config.Checkpoints) {
		return nil, errors.New("memory worker: canonical stores, view stores, DAGs, and checkpoints are required")
	}
	if (nilInterface(config.Summaries)) != (config.Compactor == nil) {
		return nil, errors.New("memory worker: summary store and compactor must be configured together")
	}
	if strings.TrimSpace(config.Projection) == "" {
		return nil, errors.New("memory worker: projection name is required")
	}
	if strings.TrimSpace(config.PolicyDigest) == "" {
		return nil, errors.New("memory worker: policy digest is required")
	}
	if len(config.Indexers) != 3 {
		return nil, fmt.Errorf("memory worker: exactly three projection indexers are required, got %d", len(config.Indexers))
	}
	seen := make(map[string]struct{}, len(config.Indexers))
	indexers := append([]ProjectionIndexer(nil), config.Indexers...)
	for index, lane := range indexers {
		if strings.TrimSpace(lane.Name) == "" || nilInterface(lane.Indexer) {
			return nil, fmt.Errorf("memory worker: indexer %d name and implementation are required", index)
		}
		if _, exists := seen[lane.Name]; exists {
			return nil, fmt.Errorf("memory worker: duplicate projection indexer %q", lane.Name)
		}
		seen[lane.Name] = struct{}{}
	}
	return &Processor{
		messages: config.Messages, documents: config.Documents, facts: config.Facts,
		summaries: config.Summaries, compactor: config.Compactor,
		documentViews: config.DocumentViews, chatDAG: config.ChatDAG,
		knowledgeDAG: config.KnowledgeDAG, checkpoints: config.Checkpoints,
		projection: config.Projection, policyDigest: config.PolicyDigest, indexers: indexers,
	}, nil
}

func (processor *Processor) ProcessScope(ctx context.Context, scope sdkmemory.Scope) error {
	return processor.processScope(ctx, scope, true)
}

// ReconcileScope explicitly scans every source item. Normal polling uses
// durable watermarks and never performs this full reconciliation implicitly.
func (processor *Processor) ReconcileScope(ctx context.Context, scope sdkmemory.Scope) error {
	return processor.processScope(ctx, scope, false)
}

func (processor *Processor) processScope(ctx context.Context, scope sdkmemory.Scope, useWatermarks bool) error {
	if processor == nil {
		return errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return errors.New("memory worker: context is required")
	}
	if err := processor.validate(); err != nil {
		return err
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	var failures []error
	conversations, err := processor.messages.ListConversations(ctx, scope)
	if err != nil {
		failures = append(failures, fmt.Errorf("scan conversations: %w", err))
	} else {
		for _, conversationID := range conversations {
			if err := processor.processConversationStream(ctx, scope, conversationID, useWatermarks); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := processor.processDocumentStream(ctx, scope, useWatermarks); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// validate reports whether the processor has all required components.
// It mirrors the NewProcessor checks so a zero-value Processor passed
// directly to Runner returns an error instead of panicking on a nil
// store interface during the first scan.
func (processor *Processor) validate() error {
	if processor == nil {
		return errors.New("memory worker: processor is required")
	}
	if nilInterface(processor.messages) || nilInterface(processor.documents) ||
		nilInterface(processor.facts) || nilInterface(processor.documentViews) ||
		processor.chatDAG == nil || processor.knowledgeDAG == nil ||
		nilInterface(processor.checkpoints) {
		return errors.New("memory worker: canonical stores, view stores, DAGs, and checkpoints are required")
	}
	if (nilInterface(processor.summaries)) != (processor.compactor == nil) {
		return errors.New("memory worker: summary store and compactor must be configured together")
	}
	if strings.TrimSpace(processor.projection) == "" {
		return errors.New("memory worker: projection name is required")
	}
	if strings.TrimSpace(processor.policyDigest) == "" {
		return errors.New("memory worker: policy digest is required")
	}
	if len(processor.indexers) != 3 {
		return fmt.Errorf("memory worker: exactly three projection indexers are required, got %d", len(processor.indexers))
	}
	return nil
}

func (processor *Processor) processConversationStream(
	ctx context.Context,
	scope sdkmemory.Scope,
	conversationID string,
	useWatermark bool,
) error {
	cursor := uint64(0)
	if useWatermark {
		watermark, ok, err := processor.checkpoints.LoadWatermark(
			ctx, scope, "message-commits", conversationID, processor.policyDigest,
		)
		if err != nil {
			return fmt.Errorf("load conversation %q watermark: %w", conversationID, err)
		}
		if ok {
			cursor = watermark.Cursor
		}
	}
	for {
		commits, err := processor.messages.ListCommits(ctx, scope, conversationID, msgsource.ListCommitOptions{
			AfterVersion: cursor, Limit: sourcePageSize,
		})
		if err != nil {
			return fmt.Errorf("scan conversation %q: %w", conversationID, err)
		}
		for _, commit := range commits {
			if err := processor.processChat(ctx, commit); err != nil {
				return fmt.Errorf("chat work %q: %w", commit.ID, err)
			}
			cursor = commit.Version
			if useWatermark {
				if err := processor.checkpoints.SaveWatermark(ctx, SourceWatermark{
					Scope: scope, StreamKind: "message-commits", StreamID: conversationID,
					PolicyDigest: processor.policyDigest, Cursor: cursor,
				}); err != nil {
					return fmt.Errorf("save conversation %q watermark: %w", conversationID, err)
				}
			}
		}
		if len(commits) < sourcePageSize {
			return nil
		}
	}
}

func (processor *Processor) processDocumentStream(
	ctx context.Context,
	scope sdkmemory.Scope,
	useWatermark bool,
) error {
	const streamID = "scope"
	cursor := uint64(0)
	if useWatermark {
		watermark, ok, err := processor.checkpoints.LoadWatermark(
			ctx, scope, "document-events", streamID, processor.policyDigest,
		)
		if err != nil {
			return fmt.Errorf("load document watermark: %w", err)
		}
		if ok {
			cursor = watermark.Cursor
		}
	}
	for {
		events, err := processor.documents.ListEvents(ctx, scope, docsource.ListEventOptions{
			AfterOutboxSeq: cursor, Limit: sourcePageSize,
		})
		if err != nil {
			return fmt.Errorf("scan document events: %w", err)
		}
		for _, event := range events {
			if err := processor.processKnowledge(ctx, event); err != nil {
				return fmt.Errorf("knowledge work %q: %w", event.ID, err)
			}
			cursor = event.OutboxSeq
			if useWatermark {
				if err := processor.checkpoints.SaveWatermark(ctx, SourceWatermark{
					Scope: scope, StreamKind: "document-events", StreamID: streamID,
					PolicyDigest: processor.policyDigest, Cursor: cursor,
				}); err != nil {
					return fmt.Errorf("save document watermark: %w", err)
				}
			}
		}
		if len(events) < sourcePageSize {
			return nil
		}
	}
}

func (processor *Processor) processChat(ctx context.Context, commit msgsource.Commit) error {
	work := WorkIdentity{Kind: "message-commit", ID: commit.ID, PolicyDigest: processor.policyDigest}
	checkpoint, exists, err := processor.checkpoints.Load(ctx, commit.Scope, work, chatBranch)
	if err != nil {
		return err
	}
	if !exists || checkpoint.Status != StatusComplete {
		source := chatSource(commit)
		result, runErr := processor.runDAG(ctx, processor.chatDAG, source, checkpoint, exists)
		if runErr != nil {
			return processor.recordFailure(ctx, commit.Scope, work, chatBranch, checkpoint.Attempt+1, nil, runErr)
		}
		if err := unsuccessfulRun(result); err != nil {
			status := StatusFailed
			if blockedRun(result) {
				status = StatusBlocked
			}
			return processor.saveUnsuccessful(ctx, commit.Scope, work, chatBranch, checkpoint.Attempt+1, status, result, err)
		}
		if err := processor.publishFacts(ctx, commit, result); err != nil {
			return processor.saveUnsuccessful(ctx, commit.Scope, work, chatBranch, checkpoint.Attempt+1, StatusFailed, result, err)
		}
		checkpoint = Checkpoint{Scope: commit.Scope, Work: work, Branch: chatBranch, Status: StatusComplete, Attempt: checkpoint.Attempt + 1, RunResult: &result}
		if err := processor.checkpoints.Save(ctx, checkpoint); err != nil {
			return err
		}
	}
	if processor.compactor != nil {
		if err := processor.processSummary(ctx, commit, work); err != nil {
			return err
		}
	}
	delta, err := processor.chatProjectionDelta(ctx, commit, checkpoint.RunResult)
	if err != nil {
		return err
	}
	return processor.applyPending(ctx, commit.Scope, work, delta)
}

func (processor *Processor) processSummary(ctx context.Context, commit msgsource.Commit, work WorkIdentity) error {
	checkpoint, exists, err := processor.checkpoints.Load(ctx, commit.Scope, work, summaryBranch)
	if err != nil {
		return err
	}
	if exists && checkpoint.Status == StatusComplete {
		return nil
	}
	facts, err := processor.facts.List(ctx, commit.Scope, commit.ConversationID, factview.ListOptions{})
	if err != nil {
		return processor.recordFailure(ctx, commit.Scope, work, summaryBranch, checkpoint.Attempt+1, nil, err)
	}
	inputs := make([]summaryderive.Input, 0, len(facts))
	for _, fact := range facts {
		startSeq, endSeq := provenanceSequenceRange(fact.Provenance)
		inputs = append(inputs, summaryderive.Input{
			ID: fact.ID, Text: fact.Text, Topics: fact.Entities,
			SourceRefs: fact.Provenance,
			CoverageRange: summaryview.CoverageRange{
				StartSeq: startSeq, EndSeq: endSeq, StartTime: fact.EventTime, EndTime: fact.EventTime,
			},
		})
	}
	_, compactErr := processor.compactor.Compact(ctx, summaryderive.CompactRequest{
		Scope: commit.Scope, ConversationID: commit.ConversationID,
		GenerationID: "message-commit:" + commit.ID, PolicySignature: processor.policyDigest, Inputs: inputs,
	})
	if compactErr != nil {
		return processor.recordFailure(ctx, commit.Scope, work, summaryBranch, checkpoint.Attempt+1, nil, compactErr)
	}
	return processor.checkpoints.Save(ctx, Checkpoint{
		Scope: commit.Scope, Work: work, Branch: summaryBranch,
		Status: StatusComplete, Attempt: checkpoint.Attempt + 1,
	})
}

func provenanceSequenceRange(sources []sdkmemory.SourceRef) (uint64, uint64) {
	var start, end uint64
	for _, source := range sources {
		if source.Kind != sdkmemory.SourceMessage {
			continue
		}
		sequence, err := strconv.ParseUint(source.Revision, 10, 64)
		if err != nil || sequence == 0 {
			continue
		}
		if start == 0 || sequence < start {
			start = sequence
		}
		if sequence > end {
			end = sequence
		}
	}
	return start, end
}

func (processor *Processor) processKnowledge(ctx context.Context, event docsource.Event) error {
	work := WorkIdentity{Kind: "document-event", ID: event.ID, PolicyDigest: processor.policyDigest}
	checkpoint, exists, err := processor.checkpoints.Load(ctx, event.Scope, work, knowledgeBranch)
	if err != nil {
		return err
	}
	if !exists || checkpoint.Status != StatusComplete {
		if event.Operation == docsource.OperationTombstone {
			if _, err := processor.documentViews.ReplaceDocument(ctx, docview.ReplaceRequest{
				Scope: event.Scope, DatasetID: event.DatasetID, DocumentID: event.DocumentID,
				DocumentVersion: event.Version, Chunks: []docview.Chunk{},
			}); err != nil {
				return processor.recordFailure(ctx, event.Scope, work, knowledgeBranch, checkpoint.Attempt+1, nil, err)
			}
			checkpoint = Checkpoint{
				Scope: event.Scope, Work: work, Branch: knowledgeBranch,
				Status: StatusComplete, Attempt: checkpoint.Attempt + 1,
			}
			if err := processor.checkpoints.Save(ctx, checkpoint); err != nil {
				return err
			}
			return processor.applyPending(ctx, event.Scope, work, component.ProjectionDelta{
				Scope: event.Scope, Projection: processor.projection,
				ReconcileDocuments: []component.DocumentAddress{{DatasetID: event.DatasetID, DocumentID: event.DocumentID}}, ActiveIDs: []string{},
				SourceRevision: work.Kind + ":" + work.ID,
				SourceDigest:   projectionSourceDigest(nil),
			})
		}
		if event.Operation != docsource.OperationPut || event.Document == nil {
			return fmt.Errorf("unsupported document event operation %q", event.Operation)
		}
		document := *event.Document
		source := documentSource(document)
		result, runErr := processor.runDAG(ctx, processor.knowledgeDAG, source, checkpoint, exists)
		if runErr != nil {
			return processor.recordFailure(ctx, event.Scope, work, knowledgeBranch, checkpoint.Attempt+1, nil, runErr)
		}
		if err := unsuccessfulRun(result); err != nil {
			status := StatusFailed
			if blockedRun(result) {
				status = StatusBlocked
			}
			return processor.saveUnsuccessful(ctx, event.Scope, work, knowledgeBranch, checkpoint.Attempt+1, status, result, err)
		}
		if err := processor.publishChunks(ctx, document, result); err != nil {
			return processor.saveUnsuccessful(ctx, event.Scope, work, knowledgeBranch, checkpoint.Attempt+1, StatusFailed, result, err)
		}
		checkpoint = Checkpoint{Scope: event.Scope, Work: work, Branch: knowledgeBranch, Status: StatusComplete, Attempt: checkpoint.Attempt + 1, RunResult: &result}
		if err := processor.checkpoints.Save(ctx, checkpoint); err != nil {
			return err
		}
	}
	delta, err := processor.knowledgeProjectionDelta(ctx, event)
	if err != nil {
		return err
	}
	return processor.applyPending(ctx, event.Scope, work, delta)
}

func (processor *Processor) runDAG(ctx context.Context, dag *derive.DAG, source component.Artifact, checkpoint Checkpoint, exists bool) (derive.RunResult, error) {
	if exists && checkpoint.RunResult != nil {
		return dag.Retry(ctx, checkpoint.RunResult.Clone())
	}
	return dag.Run(ctx, source)
}

func (processor *Processor) publishFacts(ctx context.Context, commit msgsource.Commit, result derive.RunResult) error {
	for _, node := range result.Nodes {
		for _, artifact := range node.Artifacts {
			if artifact.Kind != chat.KindFact {
				continue
			}
			metadata := withAddressMetadata(artifact.Metadata, sdkmemory.ContextFact, commit.ConversationID, "", "", artifact.ID)
			addScopeMetadata(metadata, commit.Scope)
			metadata["commit_version"] = strconv.FormatUint(commit.Version, 10)
			eventTime, _ := time.Parse(time.RFC3339Nano, artifact.Metadata["event_time"])
			if _, err := processor.facts.Add(ctx, factview.AddRequest{
				ID: artifact.ID, Scope: commit.Scope, ConversationID: commit.ConversationID,
				Content: artifact.Content, Provenance: artifact.Sources, Metadata: metadata,
				CanonicalHash: artifact.Metadata["canonical_hash"],
				Entities:      decodeMetadataStrings(artifact.Metadata["entities"]),
				Predicate:     artifact.Metadata["predicate"], TemporalDetail: artifact.Metadata["temporal_detail"],
				EventTime: eventTime, LinkedMemoryIDs: decodeMetadataStrings(artifact.Metadata["linked_memory_ids"]),
				SourceDigest:       artifact.Metadata["source_digest"],
				TransformSignature: artifact.Metadata["transform_signature"],
			}); err != nil {
				return fmt.Errorf("publish fact %q: %w", artifact.ID, err)
			}
		}
	}
	return nil
}

func (processor *Processor) publishChunks(ctx context.Context, document docsource.Document, result derive.RunResult) error {
	var chunks []docview.Chunk
	for _, node := range result.Nodes {
		for _, artifact := range node.Artifacts {
			recordKind, contextKind, ok := knowledgeRecordKinds(artifact.Kind)
			if !ok {
				continue
			}
			metadata := withAddressMetadata(artifact.Metadata, contextKind, "", document.DatasetID, document.DocumentID, artifact.ID)
			addScopeMetadata(metadata, document.Scope)
			metadata["document_version"] = strconv.FormatUint(document.Version, 10)
			level, _ := strconv.Atoi(artifact.Metadata["level"])
			ordinal, _ := strconv.ParseUint(artifact.Metadata["ordinal"], 10, 64)
			chunks = append(chunks, docview.Chunk{
				ID: artifact.ID, Kind: recordKind, Level: level, ParentID: artifact.Metadata["parent_id"],
				Scope: document.Scope, DatasetID: document.DatasetID,
				DocumentID: document.DocumentID, DocumentVersion: document.Version,
				Ordinal: ordinal, Title: artifact.Metadata["title"],
				Content: artifact.Content, Provenance: artifact.Sources,
				SourceDigest:       artifact.Metadata["source_digest"],
				TransformSignature: artifact.Metadata["transform_signature"], Metadata: metadata,
			})
		}
	}
	_, err := processor.documentViews.ReplaceDocument(ctx, docview.ReplaceRequest{
		Scope: document.Scope, DatasetID: document.DatasetID, DocumentID: document.DocumentID,
		DocumentVersion: document.Version, Chunks: chunks,
	})
	return err
}

func (processor *Processor) chatProjectionDelta(ctx context.Context, commit msgsource.Commit, result *derive.RunResult) (component.ProjectionDelta, error) {
	artifacts := make([]component.Artifact, 0, len(commit.Records))
	for _, record := range commit.Records {
		metadata := withAddressMetadata(record.Metadata, sdkmemory.ContextRawMessage, commit.ConversationID, "", "", record.ID)
		addScopeMetadata(metadata, commit.Scope)
		metadata["message_seq"] = strconv.FormatUint(record.Seq, 10)
		artifacts = append(artifacts, component.Artifact{
			Kind: chat.KindRawMessage, ID: projectionArtifactID("message", commit.ConversationID, record.ID),
			Content: record.Message.Content, Sources: []sdkmemory.SourceRef{messageSourceRef(record)}, Metadata: metadata,
		})
	}
	if result != nil {
		seen := make(map[string]struct{})
		for _, node := range result.Nodes {
			for _, derived := range node.Artifacts {
				if derived.Kind != chat.KindFact {
					continue
				}
				if _, duplicate := seen[derived.ID]; duplicate {
					continue
				}
				seen[derived.ID] = struct{}{}
				fact, found, err := processor.facts.Get(ctx, commit.Scope, commit.ConversationID, derived.ID)
				if err != nil {
					return component.ProjectionDelta{}, err
				}
				if !found {
					continue
				}
				metadata := withAddressMetadata(fact.Metadata, sdkmemory.ContextFact, commit.ConversationID, "", "", fact.ID)
				addScopeMetadata(metadata, commit.Scope)
				metadata["entities"] = encodeMetadataStrings(fact.Entities)
				artifacts = append(artifacts, component.Artifact{
					Kind: chat.KindFact, ID: projectionArtifactID("fact", commit.ConversationID, fact.ID),
					Content: fact.Content, Entities: append([]string(nil), fact.Entities...),
					Sources: fact.Provenance, Metadata: metadata,
				})
			}
		}
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].ID < artifacts[j].ID })
	return component.ProjectionDelta{
		Scope: commit.Scope, Projection: processor.projection, Upserts: artifacts,
		SourceRevision: "message-commit:" + commit.ID, SourceDigest: projectionSourceDigest(artifacts),
	}, nil
}

func (processor *Processor) knowledgeProjectionDelta(ctx context.Context, event docsource.Event) (component.ProjectionDelta, error) {
	chunks, err := processor.documentViews.List(
		ctx, event.Scope, event.DatasetID, event.DocumentID, docview.ListOptions{},
	)
	if err != nil {
		return component.ProjectionDelta{}, err
	}
	artifacts := make([]component.Artifact, 0, len(chunks))
	activeIDs := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		artifactKind, contextKind := viewRecordKinds(chunk.Kind)
		metadata := withAddressMetadata(chunk.Metadata, contextKind, "", event.DatasetID, event.DocumentID, chunk.ID)
		addScopeMetadata(metadata, event.Scope)
		metadata["document_version"] = strconv.FormatUint(chunk.DocumentVersion, 10)
		metadata["ordinal"] = strconv.FormatUint(chunk.Ordinal, 10)
		artifact := component.Artifact{
			Kind: artifactKind, ID: projectionArtifactID("chunk", event.DatasetID, event.DocumentID, chunk.ID),
			Content: chunk.Content, Sources: chunk.Provenance, Metadata: metadata,
		}
		artifacts = append(artifacts, artifact)
		activeIDs = append(activeIDs, artifact.ID)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].ID < artifacts[j].ID })
	sort.Strings(activeIDs)
	return component.ProjectionDelta{
		Scope: event.Scope, Projection: processor.projection, Upserts: artifacts,
		ReconcileDocuments: []component.DocumentAddress{{DatasetID: event.DatasetID, DocumentID: event.DocumentID}}, ActiveIDs: activeIDs,
		SourceRevision: "document-event:" + event.ID, SourceDigest: projectionSourceDigest(artifacts),
	}, nil
}

func (processor *Processor) applyPending(ctx context.Context, scope sdkmemory.Scope, work WorkIdentity, delta component.ProjectionDelta) error {
	var failures []error
	var fullArtifacts []component.Artifact
	var fullErr error
	for _, lane := range processor.indexers {
		branch := "projection-" + lane.Name
		prior, exists, loadErr := processor.checkpoints.Load(ctx, scope, work, branch)
		if loadErr != nil {
			failures = append(failures, fmt.Errorf("%s checkpoint: %w", lane.Name, loadErr))
			continue
		}
		if exists && prior.Status == StatusComplete {
			continue
		}
		attempt := prior.Attempt + 1
		var rebuildErr error
		if deltaIndexer, ok := lane.Indexer.(component.DeltaIndexer); ok {
			laneDelta := delta
			laneDelta.Upserts = component.CloneArtifacts(delta.Upserts)
			laneDelta.DeleteIDs = append([]string(nil), delta.DeleteIDs...)
			laneDelta.DeleteDocuments = append([]component.DocumentAddress(nil), delta.DeleteDocuments...)
			laneDelta.ReconcileDocuments = append([]component.DocumentAddress(nil), delta.ReconcileDocuments...)
			laneDelta.ActiveIDs = append([]string(nil), delta.ActiveIDs...)
			rebuildErr = deltaIndexer.ApplyDelta(ctx, laneDelta)
		} else {
			if fullArtifacts == nil && fullErr == nil {
				fullArtifacts, fullErr = processor.collectArtifacts(ctx, scope)
			}
			if fullErr != nil {
				rebuildErr = fullErr
			} else {
				rebuildErr = lane.Indexer.Rebuild(ctx, component.ProjectionRequest{
					Scope: scope, Projection: processor.projection, Artifacts: component.CloneArtifacts(fullArtifacts),
				})
			}
		}
		if rebuildErr != nil {
			saveErr := processor.checkpoints.Save(ctx, Checkpoint{
				Scope: scope, Work: work, Branch: branch, Status: StatusFailed,
				Attempt: attempt, Error: rebuildErr.Error(),
			})
			failures = append(failures, errors.Join(fmt.Errorf("%s rebuild: %w", lane.Name, rebuildErr), saveErr))
			continue
		}
		if saveErr := processor.checkpoints.Save(ctx, Checkpoint{
			Scope: scope, Work: work, Branch: branch, Status: StatusComplete, Attempt: attempt,
		}); saveErr != nil {
			failures = append(failures, fmt.Errorf("%s checkpoint: %w", lane.Name, saveErr))
		}
	}
	return errors.Join(failures...)
}

func (processor *Processor) collectArtifacts(ctx context.Context, scope sdkmemory.Scope) ([]component.Artifact, error) {
	var artifacts []component.Artifact
	conversations, err := processor.messages.ListConversations(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, conversationID := range conversations {
		records, err := processor.messages.List(ctx, scope, conversationID, msgsource.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			metadata := withAddressMetadata(record.Metadata, sdkmemory.ContextRawMessage, conversationID, "", "", record.ID)
			addScopeMetadata(metadata, scope)
			metadata["message_seq"] = strconv.FormatUint(record.Seq, 10)
			artifacts = append(artifacts, component.Artifact{
				Kind: chat.KindRawMessage, ID: projectionArtifactID("message", conversationID, record.ID),
				Content: record.Message.Content, Sources: []sdkmemory.SourceRef{messageSourceRef(record)}, Metadata: metadata,
			})
		}
		facts, err := processor.facts.List(ctx, scope, conversationID, factview.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, fact := range facts {
			metadata := withAddressMetadata(fact.Metadata, sdkmemory.ContextFact, conversationID, "", "", fact.ID)
			addScopeMetadata(metadata, scope)
			metadata["entities"] = encodeMetadataStrings(fact.Entities)
			metadata["linked_memory_ids"] = encodeMetadataStrings(fact.LinkedMemoryIDs)
			metadata["canonical_hash"] = fact.CanonicalHash
			metadata["event_time"] = fact.EventTime.Format(time.RFC3339Nano)
			artifacts = append(artifacts, component.Artifact{
				Kind: chat.KindFact, ID: projectionArtifactID("fact", conversationID, fact.ID),
				Content: fact.Content, Entities: append([]string(nil), fact.Entities...),
				Sources: fact.Provenance, Metadata: metadata,
			})
		}
	}
	datasets, err := processor.documents.ListDatasets(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, datasetID := range datasets {
		documents, err := processor.documents.List(ctx, scope, datasetID, docsource.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, document := range documents {
			chunks, err := processor.documentViews.List(ctx, scope, datasetID, document.DocumentID, docview.ListOptions{})
			if err != nil {
				return nil, err
			}
			for _, chunk := range chunks {
				artifactKind, contextKind := viewRecordKinds(chunk.Kind)
				metadata := withAddressMetadata(chunk.Metadata, contextKind, "", datasetID, document.DocumentID, chunk.ID)
				addScopeMetadata(metadata, scope)
				metadata["document_version"] = strconv.FormatUint(chunk.DocumentVersion, 10)
				metadata["ordinal"] = strconv.FormatUint(chunk.Ordinal, 10)
				artifacts = append(artifacts, component.Artifact{
					Kind:    artifactKind,
					ID:      projectionArtifactID("chunk", datasetID, document.DocumentID, chunk.ID),
					Content: chunk.Content, Sources: chunk.Provenance, Metadata: metadata,
				})
			}
		}
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].ID < artifacts[j].ID })
	return artifacts, nil
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

func knowledgeRecordKinds(kind component.ArtifactKind) (docview.RecordKind, sdkmemory.ContextItemKind, bool) {
	switch kind {
	case knowledge.KindResource:
		return docview.KindResource, sdkmemory.ContextDocumentResource, true
	case knowledge.KindSection:
		return docview.KindSection, sdkmemory.ContextDocumentSection, true
	case knowledge.KindChunk:
		return docview.KindChunk, sdkmemory.ContextDocumentChunk, true
	case knowledge.KindSummary:
		return docview.KindSummary, sdkmemory.ContextDocumentSummary, true
	default:
		return "", "", false
	}
}

func viewRecordKinds(kind docview.RecordKind) (component.ArtifactKind, sdkmemory.ContextItemKind) {
	switch kind {
	case docview.KindResource:
		return knowledge.KindResource, sdkmemory.ContextDocumentResource
	case docview.KindSection:
		return knowledge.KindSection, sdkmemory.ContextDocumentSection
	case docview.KindSummary:
		return knowledge.KindSummary, sdkmemory.ContextDocumentSummary
	default:
		return knowledge.KindChunk, sdkmemory.ContextDocumentChunk
	}
}

func (processor *Processor) saveUnsuccessful(ctx context.Context, scope sdkmemory.Scope, work WorkIdentity, branch string, attempt int, status CheckpointStatus, result derive.RunResult, cause error) error {
	saveErr := processor.checkpoints.Save(ctx, Checkpoint{
		Scope: scope, Work: work, Branch: branch, Status: status, Attempt: attempt,
		Error: cause.Error(), RunResult: &result,
	})
	return errors.Join(cause, saveErr)
}

func (processor *Processor) recordFailure(ctx context.Context, scope sdkmemory.Scope, work WorkIdentity, branch string, attempt int, result *derive.RunResult, cause error) error {
	saveErr := processor.checkpoints.Save(ctx, Checkpoint{
		Scope: scope, Work: work, Branch: branch, Status: StatusFailed, Attempt: attempt,
		Error: cause.Error(), RunResult: result,
	})
	return errors.Join(cause, saveErr)
}

func unsuccessfulRun(result derive.RunResult) error {
	var messages []string
	for _, node := range result.Nodes {
		if node.Status != derive.StatusSuccess {
			messages = append(messages, fmt.Sprintf("%s: %s", node.ID, node.Error))
		}
	}
	if len(messages) == 0 {
		return nil
	}
	return errors.New(strings.Join(messages, "; "))
}

func blockedRun(result derive.RunResult) bool {
	for _, node := range result.Nodes {
		if node.Status == derive.StatusFailed {
			return false
		}
	}
	return true
}

func chatSource(commit msgsource.Commit) component.Artifact {
	var text strings.Builder
	sources := make([]sdkmemory.SourceRef, 0, len(commit.Records))
	for _, record := range commit.Records {
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(string(record.Message.Role))
		text.WriteString(": ")
		text.WriteString(record.Message.Content.Text())
		sources = append(sources, messageSourceRef(record))
	}
	metadata := withAddressMetadata(commit.Records[0].Metadata, sdkmemory.ContextRawMessage, commit.ConversationID, "", "", commit.Records[0].ID)
	addScopeMetadata(metadata, commit.Scope)
	metadata["commit_id"] = commit.ID
	metadata["commit_version"] = strconv.FormatUint(commit.Version, 10)
	metadata["event_time"] = commit.CreatedAt.UTC().Format(time.RFC3339Nano)
	return component.Artifact{
		Kind: chat.KindRawMessage, ID: commit.ID,
		Content: sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text.String()}}},
		Sources: sources, Metadata: metadata,
	}
}

func documentSource(document docsource.Document) component.Artifact {
	metadata := withAddressMetadata(document.Metadata, sdkmemory.ContextDocumentChunk, "", document.DatasetID, document.DocumentID, document.DocumentID)
	addScopeMetadata(metadata, document.Scope)
	metadata["document_version"] = strconv.FormatUint(document.Version, 10)
	return component.Artifact{
		Kind: knowledge.KindDocument, ID: documentWorkID(document), Content: document.Content,
		Sources: append([]sdkmemory.SourceRef{{
			Kind: sdkmemory.SourceDocument, ID: document.DatasetID + "/" + document.DocumentID,
			Revision: strconv.FormatUint(document.Version, 10),
		}}, document.Provenance...), Metadata: metadata,
	}
}

func messageSourceRef(record msgsource.Record) sdkmemory.SourceRef {
	return sdkmemory.SourceRef{
		Kind: sdkmemory.SourceMessage, ID: record.ConversationID + "/" + record.ID,
		Revision: strconv.FormatUint(record.Seq, 10),
	}
}

func documentWorkID(document docsource.Document) string {
	return projectionArtifactID("document", document.DatasetID, document.DocumentID, strconv.FormatUint(document.Version, 10))
}

func projectionArtifactID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func withAddressMetadata(source sdkmemory.Metadata, kind sdkmemory.ContextItemKind, conversationID, datasetID, documentID, itemID string) sdkmemory.Metadata {
	metadata := make(sdkmemory.Metadata, len(source)+6)
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

func addScopeMetadata(metadata sdkmemory.Metadata, scope sdkmemory.Scope) {
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
