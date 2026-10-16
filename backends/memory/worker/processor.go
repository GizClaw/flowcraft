package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// ObservedAtAnchor renders each record's observed_at anchor as a text
	// prefix in the derivation source (memory.Settings.ObservedAtAnchor).
	ObservedAtAnchor bool
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
	messages         MessageReader
	documents        DocumentReader
	documentViews    *docview.DocumentViewStore
	facts            *factview.FactStore
	deriver          component.Deriver
	knowledge        component.Deriver
	compactor        *summaryderive.Compactor
	indexers         []ProjectionIndexer
	checkpoints      CheckpointStore
	projection       string
	policyDigest     string
	observedAtAnchor bool
	pageSize         int
	concurrency      int

	// running tracks the conversations with a pass in flight in this processor,
	// so a sweep can refuse to race one. See quiesce.go.
	runningMu sync.Mutex
	running   map[string]int
	// repairing tracks the scopes whose projection lanes a host is rebuilding,
	// so a pass refuses to index beside the rebuild. It shares runningMu with
	// running: a pass and a rebuild must not both find the scope free.
	repairing map[string]int

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
		projection: config.Projection, policyDigest: config.PolicyDigest,
		observedAtAnchor: config.ObservedAtAnchor,
		pageSize:         pageSize, concurrency: concurrency,
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
	// A scope pass refuses as a whole while the scope's lanes are being
	// rebuilt: every step of it indexes into those lanes.
	if processor.underRepair(scope) {
		return &ScopeUnderRepairError{Scope: scope}
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

// ScopeArtifactSet returns the canonical artifacts that re-index one scope's
// lanes: every stored message, the facts of each conversation's active
// generation, and the chunks of every document's active build. It reads
// canonical state and calls no deriver, so a caller that re-indexes with it
// indexes what was derived instead of deriving it again.
//
// The active generation is the one readers resolve. A fact of a generation no
// reader serves is not in the set, and neither is a superseded document build:
// re-indexing them would put entries in the lanes that hydration cannot
// resolve.
//
// The whole set is in memory by construction: a rebuild replaces a lane's
// state in one publication, so one repair costs one full re-index of the scope.
// The message log is read in pages of processor.pageSize -- the page size the
// derivation paths use -- so the set is accumulated once instead of beside a
// second copy of the log. The facts of the active generation and the chunks of
// the active builds are read whole, one call each: that is the shape their
// readers offer, and they are summed into the same set either way.
func (processor *Processor) ScopeArtifactSet(ctx context.Context, scope corememory.Scope) ([]component.Artifact, error) {
	if processor == nil {
		return nil, errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return nil, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	artifacts := make([]component.Artifact, 0)
	conversations, err := processor.messages.ListConversations(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("memory worker: list conversations: %w", err)
	}
	for _, conversationID := range conversations {
		after := uint64(0)
		for {
			records, err := processor.messages.List(ctx, scope, conversationID, msgsource.ListOptions{
				AfterSeq: after, Limit: processor.pageSize,
			})
			if err != nil {
				return nil, fmt.Errorf("memory worker: list messages of %q: %w", conversationID, err)
			}
			for _, record := range records {
				artifacts = append(artifacts, messageArtifact(scope, conversationID, record))
				if record.Seq > after {
					after = record.Seq
				}
			}
			if len(records) < processor.pageSize {
				break
			}
		}
	}
	facts, err := processor.facts.ListScope(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("memory worker: list facts: %w", err)
	}
	for _, fact := range facts {
		artifacts = append(artifacts, factArtifact(fact))
	}
	if processor.documentViews != nil {
		chunks, err := processor.documentViews.ListScope(ctx, scope)
		if err != nil {
			return nil, fmt.Errorf("memory worker: list document chunks: %w", err)
		}
		for _, chunk := range chunks {
			artifacts = append(artifacts, chunkArtifact(chunk))
		}
	}
	// One entry per address: a lane is keyed by artifact id, so an id the set
	// holds twice would be indexed twice.
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].ID < artifacts[j].ID })
	unique := artifacts[:0]
	for index, artifact := range artifacts {
		if index > 0 && artifact.ID == artifacts[index-1].ID {
			continue
		}
		unique = append(unique, artifact)
	}
	return unique, nil
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

// ProcessConversation scans commits after the stored watermark, converges the
// conversation's projection entries with the generation it is about to
// publish, and returns the number of commits processed.
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
	// The pass is announced before it touches anything and cleared however it
	// returns: a sweep reads this marker to refuse a conversation whose
	// derivation is still running, and a failed pass must not leave the
	// conversation looking busy for good.
	finished, err := processor.beginDerivation(scope, conversationID)
	if err != nil {
		return 0, err
	}
	defer finished()
	processed, err := processor.scanConversation(ctx, scope, conversationID)
	if err != nil {
		return processed, err
	}
	// The lanes are reconciled before the generation is published, not after:
	// converging and publishing are one switch, and a converge that fails
	// leaves the previous generation visible instead of publishing a
	// generation the projections do not hold yet.
	previous, published, err := processor.facts.ActiveGeneration(ctx, scope, conversationID)
	if err != nil {
		return processed, fmt.Errorf("memory worker: read derivation generation: %w", err)
	}
	if !published || previous != processor.policyDigest {
		// A switch re-projects the whole active window: an earlier switch
		// dropped the facts of the generation it replaced, and this
		// generation may own some of them. Adopting the first generation of a
		// conversation has nothing to re-project -- nothing ever pruned this
		// conversation -- but it may still have to drop what a generation
		// nobody published left in the lanes.
		if err := processor.convergeGeneration(ctx, scope, conversationID, published); err != nil {
			return processed, err
		}
	}
	if err := processor.republishSummaries(ctx, scope, conversationID); err != nil {
		return processed, err
	}
	// The generation is the policy digest itself: it is what the watermarks
	// are keyed by, so "which policy derived this" and "which generation a
	// reader resolves" are the same answer. A pass that failed above returns
	// before this point and leaves the previous generation visible.
	if err := processor.facts.PublishActiveGeneration(ctx, scope, conversationID, processor.policyDigest); err != nil {
		return processed, fmt.Errorf("memory worker: publish derivation generation: %w", err)
	}
	return processed, nil
}

// republishSummaries makes the summary branch serve the generation this pass is
// about to publish. Summaries are derived state like the lanes and are
// reconciled with the same switch, but unlike them they are not rebuilt from
// the facts: the branch of the generation being published was bookmarked when
// it published and is restored here.
//
// Without it a pass that derives nothing -- a policy that came back to a
// generation whose watermark is already at the end, which is what a rollback
// looks like -- would leave the summaries of the generation it replaced in
// place, and reads resolving the generation that is current again would find
// none of its summaries. Writing before the pointer moves means readers never
// resolve a generation whose summaries are still the replaced one's.
func (processor *Processor) republishSummaries(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
) error {
	if processor.compactor == nil {
		return nil
	}
	if _, _, err := processor.compactor.Republish(ctx, summaryderive.RepublishRequest{
		Scope: scope, ConversationID: conversationID, GenerationID: processor.policyDigest,
	}); err != nil {
		return fmt.Errorf("memory worker: republish summaries: %w", err)
	}
	return nil
}

// convergeGeneration makes the projection lanes hold the generation this pass
// is about to publish and nothing of the generations it replaces: the facts a
// replaced generation owns exclusively stop being projected, and every fact
// the active generation owns is (re-)projected when the switch can have
// pruned it. Lanes are rebuildable derived state, so the converge is expressed
// as one ordinary delta per lane rather than as a rebuild: it is idempotent,
// content-addressed, and leaves the lanes' manifests describing themselves.
func (processor *Processor) convergeGeneration(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	reproject bool,
) error {
	active, err := processor.facts.List(ctx, scope, conversationID, factview.ListOptions{
		Generation: processor.policyDigest,
	})
	if err != nil {
		return fmt.Errorf("memory worker: list the generation being published: %w", err)
	}
	keep := make(map[string]struct{}, len(active))
	for _, fact := range active {
		keep[fact.ID] = struct{}{}
	}
	generations, err := processor.facts.ListGenerations(ctx, scope, conversationID)
	if err != nil {
		return fmt.Errorf("memory worker: list derivation generations: %w", err)
	}
	// A fact two generations share is still live, so it stays projected: both
	// generations address that content the same way, and re-deriving it under
	// a new policy must not remove it.
	var stale []string
	for _, generation := range generations {
		if generation == processor.policyDigest {
			continue
		}
		replaced, err := processor.facts.List(ctx, scope, conversationID, factview.ListOptions{Generation: generation})
		if err != nil {
			return fmt.Errorf("memory worker: list the replaced generation %q: %w", generation, err)
		}
		for _, fact := range replaced {
			if _, live := keep[fact.ID]; live {
				continue
			}
			stale = append(stale, factLaneID(conversationID, fact.ID))
		}
	}
	if !reproject && len(stale) == 0 {
		// The conversation is adopting a generation nothing can have pruned,
		// and no other generation is stored: the lanes already hold exactly
		// this generation.
		return nil
	}
	var artifacts []component.Artifact
	if reproject {
		artifacts = make([]component.Artifact, 0, len(active))
		for _, fact := range active {
			artifacts = append(artifacts, factArtifact(fact))
		}
		sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].ID < artifacts[j].ID })
	}
	sort.Strings(stale)
	delta := component.ProjectionDelta{
		Scope: scope, Projection: processor.projection,
		Upserts: artifacts, DeleteIDs: stale,
		SourceRevision: "generation:" + processor.policyDigest,
	}
	if len(artifacts) > 0 {
		delta.SourceDigest = projectionSourceDigest(artifacts)
	}
	for _, lane := range processor.indexers {
		if err := lane.Indexer.ApplyDelta(ctx, delta); err != nil {
			return fmt.Errorf("memory worker: converge %q with the published generation: %w", lane.Name, err)
		}
		processor.bump(func(stats *Stats) { stats.IndexDeltasApplied++ })
	}
	processor.bump(func(stats *Stats) { stats.GenerationConvergences++ })
	return nil
}

// PurgeGenerations removes the projection entries of the named generations
// from every lane, and reports how many addresses it dropped; the same set
// leaves each configured lane.
//
// Retention calls this before it retires those generations, because after
// retirement their facts are gone and the addresses those entries were written
// under can no longer be enumerated: a converge walks the stored generations,
// so an entry whose generation left the fact view is one no pass can ever find
// again. Purging first is also what makes the sweep retryable -- the entries of
// a generation that is still stored are simply dropped again -- and it is safe
// on its own: the lanes serve the published generation, so entries belonging to
// any other one are stale whether or not the sweep reaches the facts.
//
// Entries the active generation also owns stay: a fact is a content address, so
// a generation that re-derived the same content keeps projecting it.
func (processor *Processor) PurgeGenerations(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	generations []string,
) (int, error) {
	if processor == nil {
		return 0, errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return 0, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return 0, errors.New("memory worker: conversation id is required")
	}
	if len(processor.indexers) == 0 {
		return 0, nil
	}
	names := make([]string, 0, len(generations))
	for _, generation := range generations {
		if generation = strings.TrimSpace(generation); generation != "" {
			names = append(names, generation)
		}
	}
	if len(names) == 0 {
		return 0, nil
	}
	sort.Strings(names)
	names = slices.Compact(names)
	live := make(map[string]struct{})
	if active, found, err := processor.facts.ActiveGeneration(ctx, scope, conversationID); err != nil {
		return 0, fmt.Errorf("memory worker: read the active generation: %w", err)
	} else if found {
		published, err := processor.facts.List(ctx, scope, conversationID, factview.ListOptions{Generation: active})
		if err != nil {
			return 0, fmt.Errorf("memory worker: list the active generation %q: %w", active, err)
		}
		for _, fact := range published {
			live[fact.ID] = struct{}{}
		}
	}
	stale := make([]string, 0, len(names))
	for _, generation := range names {
		retired, err := processor.facts.List(ctx, scope, conversationID, factview.ListOptions{Generation: generation})
		if err != nil {
			return 0, fmt.Errorf("memory worker: list the replaced generation %q: %w", generation, err)
		}
		for _, fact := range retired {
			if _, shared := live[fact.ID]; shared {
				continue
			}
			stale = append(stale, factLaneID(conversationID, fact.ID))
		}
	}
	sort.Strings(stale)
	stale = slices.Compact(stale)
	if len(stale) == 0 {
		return 0, nil
	}
	delta := component.ProjectionDelta{
		Scope: scope, Projection: processor.projection,
		DeleteIDs:      stale,
		SourceRevision: "retire:" + strings.Join(names, ","),
	}
	for _, lane := range processor.indexers {
		if err := lane.Indexer.ApplyDelta(ctx, delta); err != nil {
			return 0, fmt.Errorf("memory worker: purge %q: %w", lane.Name, err)
		}
		processor.bump(func(stats *Stats) { stats.IndexDeltasApplied++ })
	}
	return len(stale), nil
}

// RetireProgress drops the derivation progress of the named generations for one
// conversation, and reports how many watermarks it removed.
//
// It is the other half of retiring a generation, and the half that cannot be
// left behind: the watermark is where the next pass under that policy resumes,
// so a generation whose facts, lane entries and summary bookmark are retired
// while its cursor still points past them is one a rollback re-derives only in
// part -- the commits the cursor passed are never derived again, and the
// generation readers resolve is missing the facts of exactly those commits.
// Dropping the cursor makes the next pass under that policy start at the head
// of the stream, which is what "derive this generation" means once what it
// derived is gone.
//
// The conversation's published generation is refused. Reads resolve it, so its
// progress is what keeps an ordinary pass from re-deriving a generation that is
// already visible: the content of a re-derivation is the model's, so the visible
// generation would grow with whatever a second pass produces instead of
// converging. A sweep never names it -- it retires what the views do not serve
// -- so the refusal only reaches a caller that asks for it directly.
func (processor *Processor) RetireProgress(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	generations []string,
) (int, error) {
	if processor == nil {
		return 0, errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return 0, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return 0, errors.New("memory worker: conversation id is required")
	}
	digests := make([]string, 0, len(generations))
	for _, generation := range generations {
		if generation = strings.TrimSpace(generation); generation != "" {
			digests = append(digests, generation)
		}
	}
	if len(digests) == 0 {
		return 0, nil
	}
	sort.Strings(digests)
	digests = slices.Compact(digests)
	if active, found, err := processor.facts.ActiveGeneration(ctx, scope, conversationID); err != nil {
		return 0, fmt.Errorf("memory worker: read the active generation: %w", err)
	} else if found && slices.Contains(digests, active) {
		return 0, fmt.Errorf(
			"memory worker: generation %q is the published generation of conversation %q", active, conversationID)
	}
	// Only the message stream carries a conversation's progress: the document
	// stream is one cursor per scope, shared by every conversation that derives
	// knowledge from it, and a sweep over one conversation has no business
	// rewinding the documents of the others.
	return processor.checkpoints.RetireWatermarks(ctx, scope, streamKindMessages, conversationID, digests)
}

// scanConversation walks the commits after the stored watermark, deriving and
// indexing each one in order.
func (processor *Processor) scanConversation(ctx context.Context, scope corememory.Scope, conversationID string) (int, error) {
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

// ProcessDocuments runs one pass over the scope's document outbox, the
// document half of a scope pass. The sweep runs it beside the conversations it
// scans; a caller that heard about one document -- the publication trigger --
// runs it for that document's scope, which is the same pass over the same
// cursor.
//
// An assembly without document derivation reports no work rather than leaving
// the caller with a pass that never runs: there is nothing for a document to
// be derived into.
func (processor *Processor) ProcessDocuments(ctx context.Context, scope corememory.Scope) error {
	if processor == nil {
		return errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	if nilInterface(processor.documents) || nilInterface(processor.knowledge) {
		return nil
	}
	return processor.processDocumentStream(ctx, scope)
}

// processDocumentStream scans canonical document events after the stored
// scope-wide outbox cursor.
func (processor *Processor) processDocumentStream(ctx context.Context, scope corememory.Scope) error {
	// The document pass indexes into the same lanes the conversation passes
	// do, so it announces itself under the stream's own id: a rebuild refuses
	// while it runs, and it refuses while a rebuild holds the scope.
	finished, err := processor.beginDerivation(scope, documentStreamID)
	if err != nil {
		return err
	}
	defer finished()
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

// chunkArtifact renders one stored document chunk for the lanes. The chunk
// carries the document it came from and the build it belongs to, so a rebuild
// reads the same artifact out of the view alone.
func chunkArtifact(chunk docview.Chunk) component.Artifact {
	artifactKind, contextKind := viewRecordKinds(chunk.Kind)
	metadata := addressMetadata(chunk.Metadata, contextKind, "", chunk.DatasetID, chunk.DocumentID, chunk.ID)
	addScopeMetadata(metadata, chunk.Scope)
	metadata["document_version"] = strconv.FormatUint(chunk.DocumentVersion, 10)
	metadata["ordinal"] = strconv.FormatUint(chunk.Ordinal, 10)
	return component.Artifact{
		Kind:     artifactKind,
		ID:       projectionArtifactID("chunk", chunk.DatasetID, chunk.DocumentID, chunk.ID),
		Content:  chunk.Content.Clone(),
		Sources:  append([]corememory.SourceRef(nil), chunk.Provenance...),
		Metadata: metadata,
	}
}

func chunkArtifacts(event docsource.Event, chunks []docview.Chunk) ([]component.Artifact, []string) {
	artifacts := make([]component.Artifact, 0, len(chunks))
	activeIDs := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		artifact := chunkArtifact(chunk)
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
		artifacts = append(artifacts, messageArtifact(commit.Scope, commit.ConversationID, record))
	}
	if processor.deriver == nil {
		return artifacts, nil
	}
	derived, err := processor.deriver.Derive(ctx, chatSource(commit, processor.policyDigest, processor.observedAtAnchor))
	if err != nil {
		return nil, fmt.Errorf("memory worker: derive commit %q: %w", commit.ID, err)
	}
	written := make([]factview.Fact, 0, len(derived))
	writtenAt := make(map[string]int, len(derived))
	replaced := false
	for _, artifact := range derived {
		if artifact.Kind != chat.KindFact {
			continue
		}
		fact, err := processor.facts.Add(ctx, factAddRequest(commit, artifact, processor.policyDigest))
		if err != nil {
			return nil, fmt.Errorf("memory worker: publish fact %q: %w", artifact.ID, err)
		}
		if fact.ID != artifact.ID {
			// The content the deriver produced already existed under another
			// fact: this commit changed a fact the summary tree may already
			// name.
			replaced = true
		}
		processor.bump(func(stats *Stats) { stats.FactsPublished++ })
		artifacts = append(artifacts, factArtifact(fact))
		if at, duplicate := writtenAt[fact.ID]; duplicate {
			// The deriver named one fact twice: the compaction takes it once,
			// in the state the last write left it.
			written[at] = fact
			continue
		}
		writtenAt[fact.ID] = len(written)
		written = append(written, fact)
	}
	if err := processor.compact(ctx, commit, written, replaced); err != nil {
		return nil, err
	}
	return artifacts, nil
}

// compact refreshes the summary branch after one commit, appending the commit's
// own facts to the published tree.
//
// The tree below the change is reused by identity: an extension reads the
// manifest, the tail group of every level and the children that group shares
// with the rebuilt region, so what one commit costs the summary branch follows
// the commit and not the conversation. Two things send a pass back to the
// complete window instead, which is always correct: a commit that changed a
// fact the tree already names (the fact view merges identical content into the
// fact that holds it, and the tree addresses facts, not identities, so the
// group that fact sits in has to be rebuilt), and a tree the extension cannot
// place the commit's facts in (another generation's, one published before the
// tree was recorded, a window the facts do not continue).
//
// Compaction runs before the watermark advances so a failure replays
// derivation; fact publication is idempotent by canonical hash.
func (processor *Processor) compact(
	ctx context.Context,
	commit msgsource.Commit,
	written []factview.Fact,
	replaced bool,
) error {
	if processor.compactor == nil || len(written) == 0 {
		// A commit that derives no fact changes no summary: every fact the
		// conversation holds was appended by the pass that wrote it.
		return nil
	}
	if !replaced {
		inputs := make([]summaryderive.Input, 0, len(written))
		for _, value := range written {
			inputs = append(inputs, summaryderive.InputFromFact(value))
		}
		_, extended, err := processor.compactor.Extend(ctx, summaryderive.ExtendRequest{
			Scope: commit.Scope, ConversationID: commit.ConversationID,
			// The signature is part of every record id: an extension has to
			// name the leaves the compaction that built the tree named, and the
			// manifest identifies the same generation.
			GenerationID: processor.policyDigest, PolicySignature: processor.policyDigest,
			Inputs: inputs,
		})
		if err != nil {
			return fmt.Errorf("memory worker: extend summaries: %w", err)
		}
		if extended {
			processor.bump(func(stats *Stats) { stats.SummaryExtensions++ })
			return nil
		}
	}
	// Either the extension could not place the commit's facts in the published
	// tree, or the commit changed a fact the tree already names: both are
	// answered by compacting the complete window.
	processor.bump(func(stats *Stats) { stats.SummaryRebuilds++ })
	// Compaction summarises the complete fact window of the generation this
	// pass is building, never the active one: during a re-derivation the active
	// generation is the one being replaced, and summarising it is exactly how
	// the previous policy's facts survived into the new policy's summaries.
	values, err := processor.facts.List(ctx, commit.Scope, commit.ConversationID, factview.ListOptions{
		Generation: processor.policyDigest,
	})
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
		// The summary generation is the derivation generation: the same
		// identity keys the watermarks, names the fact generation, and labels
		// the summary manifest, so a reader can tell that every derived view of
		// a conversation came from one policy.
		GenerationID: processor.policyDigest, PolicySignature: processor.policyDigest,
		Inputs: inputs,
	}); err != nil {
		return fmt.Errorf("memory worker: compact summaries: %w", err)
	}
	return nil
}

// chatSource renders the derivation input for one commit. With observedAtAnchor
// set, a record that carries the observed_at anchor is rendered as
// "[anchor] role: content" -- the shape the eval loader's date prefix uses --
// and a record without one is rendered bare: the anchor is the message's own
// observation time or nothing, never the commit time.
func chatSource(commit msgsource.Commit, generation string, observedAtAnchor bool) component.Artifact {
	var text strings.Builder
	sources := make([]corememory.SourceRef, 0, len(commit.Records))
	for _, record := range commit.Records {
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		if observedAtAnchor {
			if anchor := record.ObservedAt(); anchor != "" {
				text.WriteByte('[')
				text.WriteString(anchor)
				text.WriteString("] ")
			}
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
	// The extractor links a new fact to the facts of the generation it is
	// building, so the generation travels with the source instead of being
	// re-derived downstream.
	metadata[component.GenerationMetadataKey] = generation
	return component.Artifact{
		Kind:    chat.KindRawMessage,
		ID:      commit.ID,
		Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text.String()}}},
		Sources: sources, Metadata: metadata,
	}
}

// messageArtifact renders one stored message for the lanes. The commit it
// arrived in carries nothing the lanes index beyond the scope and conversation
// it belongs to, so a rebuild reads the same artifact out of the message store
// alone.
func messageArtifact(scope corememory.Scope, conversationID string, record msgsource.Record) component.Artifact {
	metadata := addressMetadata(record.Metadata, corememory.ContextRawMessage, conversationID, "", "", record.ID)
	addScopeMetadata(metadata, scope)
	metadata["message_seq"] = strconv.FormatUint(record.Seq, 10)
	return component.Artifact{
		Kind: chat.KindRawMessage, ID: projectionArtifactID("message", conversationID, record.ID),
		Content:  record.Message.Content.Clone(),
		Sources:  []corememory.SourceRef{messageSourceRef(record)},
		Metadata: metadata,
	}
}

func factAddRequest(commit msgsource.Commit, artifact component.Artifact, generation string) factview.AddRequest {
	metadata := addressMetadata(artifact.Metadata, corememory.ContextFact, commit.ConversationID, "", "", artifact.ID)
	addScopeMetadata(metadata, commit.Scope)
	metadata["commit_version"] = strconv.FormatUint(commit.Version, 10)
	ingestedAt := commit.CreatedAt.UTC()
	eventTime, _ := time.Parse(time.RFC3339Nano, artifact.Metadata["event_time"])
	if eventTime.IsZero() {
		// A deriver that could not date the fact leaves event_time unset; the
		// commit's creation time is the only timestamp we actually know. The
		// fact keeps it as its ingestion time and marks the event time as an
		// inference, so a reader can tell a dated fact from an undated one
		// instead of reading the fallback as a stated date. Dating it 1970
		// would make it look simultaneous with every other undated fact and
		// decay it to nothing.
		eventTime = ingestedAt
		metadata[factview.EventTimeInferredMetadataKey] = "true"
	}
	return factview.AddRequest{
		ID: artifact.ID, Generation: generation, Scope: commit.Scope, ConversationID: commit.ConversationID,
		Content: artifact.Content, Provenance: artifact.Sources, Metadata: metadata,
		CanonicalHash: artifact.Metadata["canonical_hash"],
		Entities:      decodeMetadataStrings(artifact.Metadata["entities"]),
		Predicate:     artifact.Metadata["predicate"], TemporalDetail: artifact.Metadata["temporal_detail"],
		EventTime: eventTime, IngestedAt: ingestedAt, LinkedMemoryIDs: decodeMetadataStrings(artifact.Metadata["linked_memory_ids"]),
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
		Kind: chat.KindFact, ID: factLaneID(fact.ConversationID, fact.ID),
		Content: fact.Content.Clone(), Entities: append([]string(nil), fact.Entities...),
		Sources:  append([]corememory.SourceRef(nil), fact.Provenance...),
		Metadata: metadata,
	}
}

// factLaneID names one fact inside a projection lane. A fact id is a content
// address -- two conversations, or two generations, that derive the same text
// derive the same one -- while a lane is partitioned by scope. Addressing a
// fact by its content alone would therefore let a write from one conversation
// claim the entry of another, and would leave a generation switch unable to
// tell whose entry it is deleting. The address carries the conversation (the
// message and chunk lanes already do this), and item_id keeps the fact id the
// read side hydrates by.
func factLaneID(conversationID, factID string) string {
	return projectionArtifactID("fact", conversationID, factID)
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
