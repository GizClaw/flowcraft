package history

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

// Background work timeouts. Ingest and archive used to share a single 60s
// budget; that meant a slow LLM summarization could starve the archive
// step (or vice-versa). They now run independently with their own ceilings.
const (
	defaultIngestTimeout  = 60 * time.Second
	defaultArchiveTimeout = 60 * time.Second
)

// compactor is the DAG-summarization implementation of [History]. It is
// unexported on purpose: callers construct it via [NewCompacted] and
// consume it through the [History] interface. The "compactor" name
// reflects what the implementation actually does — archive + leaf
// prune discard data on disk in exchange for context-window budget;
// the previous "lossless" name was aspirational.
//
// Concurrency model: every Append acquires a per-conversation lock and
// then schedules background DAG ingest + archive on a goroutine.
// Because the per-conversation lock serializes Appends for the same
// conversationID, no two background workers race on the same DAG;
// cross-conversation background work runs in parallel without
// artificial bounds.
type compactor struct {
	store  Store
	dag    *SummaryDAG
	config DAGConfig
	ws     workspace.Workspace
	prefix string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	wg    sync.WaitGroup
}

func newCompactor(store Store, dag *SummaryDAG, config DAGConfig, ws workspace.Workspace, prefix string) *compactor {
	return &compactor{
		store:  store,
		dag:    dag,
		config: config,
		ws:     ws,
		prefix: prefix,
		locks:  make(map[string]*sync.Mutex),
	}
}

func (m *compactor) Load(ctx context.Context, conversationID string, budget Budget) ([]model.Message, error) {
	tokenBudget := m.config.TokenBudget
	if budget.MaxTokens > 0 && (tokenBudget == 0 || budget.MaxTokens < tokenBudget) {
		tokenBudget = budget.MaxTokens
	}
	msgs, err := m.dag.Assemble(ctx, conversationID, tokenBudget)
	if err != nil {
		return nil, err
	}
	if budget.MaxMessages > 0 && len(msgs) > budget.MaxMessages {
		msgs = msgs[len(msgs)-budget.MaxMessages:]
	}
	return msgs, nil
}

// Append persists newMessages and asynchronously ingests them into the
// summary DAG and (if a workspace is wired) the archive.
//
// Two correctness properties matter:
//
//  1. No read-modify-write race. The previous Save took a full history,
//     diffed it against GetMessages, and then SaveMessages-overwrote.
//     Two concurrent callers could both observe the pre-state, both
//     diff, and both overwrite — losing one side's writes. Append takes
//     the per-conversation lock and either calls MessageAppender (if the
//     Store supports it) or performs the GetMessages+concat+SaveMessages
//     fallback under the lock, so the ABA window is closed.
//
//  2. No silent ingest drop. The previous implementation used a global
//     semaphore and dropped DAG ingest when full, which silently shrank
//     the summarized history (and thereby the assembled context) under
//     load. Per-conversation serialization means the only contention is
//     between successive Appends for the same conversation, which is
//     naturally bounded; we just wg.Add them and let the workers run.
func (m *compactor) Append(ctx context.Context, conversationID string, newMessages []model.Message) error {
	if len(newMessages) == 0 {
		return nil
	}

	convMu := m.convMu(conversationID)
	convMu.Lock()
	defer convMu.Unlock()

	startSeq, err := m.persistAppend(ctx, conversationID, newMessages)
	if err != nil {
		return err
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		// Ingest and archive each get their own detached context with
		// independent timeouts so a slow LLM summarization cannot starve
		// archive (and vice-versa).
		ingestCtx, cancelIngest := context.WithTimeout(context.WithoutCancel(ctx), defaultIngestTimeout)
		defer cancelIngest()
		if err := m.dag.Ingest(ingestCtx, conversationID, newMessages, startSeq); err != nil {
			telemetry.Warn(ingestCtx, "lossless: async ingest failed",
				otellog.String("conversation_id", conversationID),
				otellog.String("error", err.Error()))
		}

		if m.ws == nil {
			return
		}
		archiveCtx, cancelArchive := context.WithTimeout(context.WithoutCancel(ctx), defaultArchiveTimeout)
		defer cancelArchive()
		if _, err := Archive(archiveCtx, m.ws, m.store, m.prefix, conversationID, m.config.Archive); err != nil {
			telemetry.Warn(archiveCtx, "lossless: async archive failed",
				otellog.String("conversation_id", conversationID),
				otellog.String("error", err.Error()))
		}
	}()

	return nil
}

// persistAppend writes newMessages to the underlying store and returns the
// 0-based sequence number where the first new message lives. Caller must
// hold the per-conversation lock.
func (m *compactor) persistAppend(ctx context.Context, conversationID string, newMessages []model.Message) (int, error) {
	if appender, ok := m.store.(MessageAppender); ok {
		// We still need the prior count for the DAG sequence. Stores that
		// support AppendMessages typically also support reading the count
		// cheaply; if that ever becomes a hotspot, add a CountMessages
		// optional interface.
		existing, err := m.store.GetMessages(ctx, conversationID)
		if err != nil {
			return 0, err
		}
		if err := appender.AppendMessages(ctx, conversationID, newMessages); err != nil {
			return 0, err
		}
		return len(existing), nil
	}

	existing, err := m.store.GetMessages(ctx, conversationID)
	if err != nil {
		return 0, err
	}
	combined := make([]model.Message, 0, len(existing)+len(newMessages))
	combined = append(combined, existing...)
	combined = append(combined, newMessages...)
	if err := m.store.SaveMessages(ctx, conversationID, combined); err != nil {
		return 0, err
	}
	return len(existing), nil
}

func (m *compactor) Clear(ctx context.Context, conversationID string) error {
	convMu := m.convMu(conversationID)
	convMu.Lock()
	defer convMu.Unlock()

	if err := m.store.DeleteMessages(ctx, conversationID); err != nil {
		return err
	}
	return m.dag.store.Rewrite(ctx, conversationID, nil)
}

// Close waits for all pending async ingest/archive goroutines to complete.
func (m *compactor) Close() {
	m.wg.Wait()
}

// Archive manually triggers archiving for this conversation.
func (m *compactor) Archive(ctx context.Context, conversationID string) (ArchiveResult, error) {
	if m.ws == nil {
		return ArchiveResult{}, nil
	}
	convMu := m.convMu(conversationID)
	convMu.Lock()
	defer convMu.Unlock()
	return Archive(ctx, m.ws, m.store, m.prefix, conversationID, m.config.Archive)
}

func (m *compactor) convMu(convID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	mu, ok := m.locks[convID]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[convID] = mu
	}
	return mu
}

// CompactOption customizes a [History] built by [NewCompacted].
//
// Compaction knobs (chunk size, recent ratio, leaf pruning, archive
// threshold, …) used to live in a dedicated Config struct; they are
// now functional options so adding a new knob does not break every
// caller passing a struct literal.
type CompactOption func(*compactOptions)

type compactOptions struct {
	dag     DAGConfig
	counter TokenCounter
	prefix  string
}

// WithDAGConfig overrides the entire [DAGConfig] used by the DAG
// summarizer. Individual knobs below compose on top of a default
// config; use this when you need to set many at once or inherit from a
// [DefaultDAGConfig].
func WithDAGConfig(cfg DAGConfig) CompactOption {
	return func(o *compactOptions) { o.dag = cfg }
}

// WithChunkSize sets how many messages feed into each leaf summary.
func WithChunkSize(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.ChunkSize = n
		}
	}
}

// WithCondenseThreshold sets the sibling-count that triggers a depth+1
// condensation.
func WithCondenseThreshold(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.CondenseThreshold = n
		}
	}
}

// WithMaxDepth caps the summary tree height.
func WithMaxDepth(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.MaxDepth = n
		}
	}
}

// WithTokenBudget caps the assembled context size in estimated tokens.
func WithTokenBudget(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.TokenBudget = n
		}
	}
}

// WithRecentRatio splits the token budget between "recent verbatim
// messages" and "older summaries".
func WithRecentRatio(r float64) CompactOption {
	return func(o *compactOptions) {
		if r > 0 {
			o.dag.RecentRatio = r
		}
	}
}

// WithCompactThreshold triggers compaction once the hot message count
// crosses this number.
func WithCompactThreshold(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.Compact.CompactThreshold = n
		}
	}
}

// WithLeafPrune turns on/off deleting the leaf content after its
// summary is absorbed into a parent node.
func WithLeafPrune(b bool) CompactOption {
	return func(o *compactOptions) { o.dag.Compact.PruneLeafContent = b }
}

// WithArchiveThreshold sets the hot-message count that triggers
// archival of the oldest batch to cold storage.
func WithArchiveThreshold(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.Archive.ArchiveThreshold = n
		}
	}
}

// WithArchiveBatchSize sets how many messages move per archive run.
func WithArchiveBatchSize(n int) CompactOption {
	return func(o *compactOptions) {
		if n > 0 {
			o.dag.Archive.ArchiveBatchSize = n
		}
	}
}

// WithTokenCounter swaps the [TokenCounter] used during assembly.
// Defaults to [EstimateCounter].
func WithTokenCounter(c TokenCounter) CompactOption {
	return func(o *compactOptions) {
		if c != nil {
			o.counter = c
		}
	}
}

// WithStoragePrefix sets the workspace prefix for summary/archive
// files. Default "memory" for backwards compatibility with files
// written by prior builds.
func WithStoragePrefix(p string) CompactOption {
	return func(o *compactOptions) {
		if p != "" {
			o.prefix = p
		}
	}
}

// NewCompacted returns a [History] that keeps the full transcript but
// summarizes older turns through a DAG to stay within a token budget.
// Requires both an LLM (for summarization) and a [workspace.Workspace]
// (for summary + archive persistence).
//
// This is the recommended default for any agent that holds
// multi-session conversations; use [NewBuffer] for short or
// single-turn interactions.
func NewCompacted(store Store, l llm.LLM, ws workspace.Workspace, opts ...CompactOption) History {
	if store == nil {
		store = NewInMemoryStore()
	}
	o := compactOptions{
		dag:     DefaultDAGConfig(),
		counter: &EstimateCounter{},
		prefix:  "memory",
	}
	for _, opt := range opts {
		opt(&o)
	}
	summaryStore := NewFileSummaryStore(ws, o.prefix)
	dag := NewSummaryDAG(summaryStore, store, l, o.dag, o.counter)
	return newCompactor(store, dag, o.dag, ws, o.prefix)
}

// ===== SummaryDAG (was compactor_dag.go) =====

var (
	dagMeter = telemetry.MeterWithSuffix("memory.dag")

	dagIngestDuration, _   = dagMeter.Float64Histogram("ingest_duration", metric.WithDescription("DAG ingest duration in seconds"))
	dagCondenseDuration, _ = dagMeter.Float64Histogram("condense_duration", metric.WithDescription("DAG condense duration in seconds"))
	dagAssembleDuration, _ = dagMeter.Float64Histogram("assemble_duration", metric.WithDescription("DAG assemble duration in seconds"))
	dagCompactDuration, _  = dagMeter.Float64Histogram("compact_duration", metric.WithDescription("DAG compact duration in seconds"))
	dagFallbackTotal, _    = dagMeter.Int64Counter("fallback_total", metric.WithDescription("DAG fallback count"))
	dagNodesTotal, _       = dagMeter.Int64Counter("nodes_total", metric.WithDescription("Total DAG nodes created"))
	dagCompactPruned, _    = dagMeter.Int64Counter("compact_pruned_total", metric.WithDescription("Total pruned d0 nodes"))
)

// CompactConfig controls the compact behavior.
type CompactConfig struct {
	CompactThreshold int
	PruneLeafContent bool
	RequireParent    bool
}

// ArchiveConfig controls message archiving behavior.
type ArchiveConfig struct {
	ArchiveThreshold int
	ArchiveBatchSize int
	ArchivePrefix    string
}

// DAGConfig controls the summary DAG behavior.
type DAGConfig struct {
	ChunkSize         int
	CondenseThreshold int
	CondenseGroupSize int
	MaxDepth          int
	TokenBudget       int
	RecentRatio       float64
	MidRatio          float64
	Compact           CompactConfig
	Archive           ArchiveConfig
}

// DefaultDAGConfig returns a DAGConfig with sensible defaults.
func DefaultDAGConfig() DAGConfig {
	return DAGConfig{
		ChunkSize:         10,
		CondenseThreshold: 6,
		CondenseGroupSize: 3,
		MaxDepth:          4,
		TokenBudget:       4000,
		RecentRatio:       0.6,
		MidRatio:          0.3,
		Compact: CompactConfig{
			CompactThreshold: 200,
			PruneLeafContent: true,
			RequireParent:    true,
		},
		Archive: ArchiveConfig{
			ArchiveThreshold: 1000,
			ArchiveBatchSize: 500,
			ArchivePrefix:    "archive",
		},
	}
}

// CompactResult holds the result of a compact operation.
type CompactResult struct {
	DeletedRemoved int `json:"deleted_removed"`
	LeafPruned     int `json:"leaf_pruned"`
	TotalRemaining int `json:"total_remaining"`
}

// SummaryDAG manages the multi-layer summary DAG for a conversation.
type SummaryDAG struct {
	store    SummaryStore
	msgStore Store
	llm      llm.LLM
	config   DAGConfig
	counter  TokenCounter
}

// NewSummaryDAG creates a new SummaryDAG.
func NewSummaryDAG(store SummaryStore, msgStore Store, l llm.LLM, cfg DAGConfig, counter TokenCounter) *SummaryDAG {
	if counter == nil {
		counter = &EstimateCounter{}
	}
	return &SummaryDAG{
		store:    store,
		msgStore: msgStore,
		llm:      l,
		config:   cfg,
		counter:  counter,
	}
}

// Ingest processes new messages and generates leaf summaries.
func (d *SummaryDAG) Ingest(ctx context.Context, convID string, messages []llm.Message, startSeq int) error {
	start := time.Now()
	defer func() {
		dagIngestDuration.Record(ctx, time.Since(start).Seconds())
	}()

	ctx, span := telemetry.Tracer().Start(ctx, "memory.dag.ingest")
	defer span.End()

	// Filter out system messages for summarization.
	var filtered []llm.Message
	var filteredSeqs []int
	for i, msg := range messages {
		if msg.Role != llm.RoleSystem {
			filtered = append(filtered, msg)
			filteredSeqs = append(filteredSeqs, startSeq+i)
		}
	}

	if len(filtered) == 0 {
		return nil
	}

	// Group by ChunkSize.
	chunkSize := d.config.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 10
	}

	for i := 0; i < len(filtered); i += chunkSize {
		end := i + chunkSize
		if end > len(filtered) {
			end = len(filtered)
		}
		chunk := filtered[i:end]
		chunkSeqs := filteredSeqs[i:end]

		earliestSeq := chunkSeqs[0]
		latestSeq := chunkSeqs[len(chunkSeqs)-1]

		content, expandHint, err := d.summarizeWithFallback(ctx, chunk, 0)
		if err != nil {
			telemetry.Warn(ctx, "dag: ingest summarize failed, using fallback",
				otellog.String("error", err.Error()))
			continue
		}

		tokenCount := d.counter.Count(content)

		node := &SummaryNode{
			ID:             NewSummaryNodeID(),
			ConversationID: convID,
			Depth:          0,
			Content:        content,
			ExpandHint:     expandHint,
			EarliestSeq:    earliestSeq,
			LatestSeq:      latestSeq,
			TokenCount:     tokenCount,
			CreatedAt:      time.Now(),
		}

		if err := d.store.Save(ctx, node); err != nil {
			telemetry.Warn(ctx, "dag: save leaf failed", otellog.String("error", err.Error()))
			continue
		}
		dagNodesTotal.Add(ctx, 1)
	}

	// Check if condense is needed.
	depth0 := intPtr(0)
	d0Nodes, err := d.store.List(ctx, convID, SummaryListOptions{Depth: depth0})
	if err == nil && len(d0Nodes) >= d.config.CondenseThreshold {
		if err := d.condense(ctx, convID, 0); err != nil {
			telemetry.Warn(ctx, "dag: condense after ingest failed", otellog.String("error", err.Error()))
		}
	}

	return nil
}

func (d *SummaryDAG) condense(ctx context.Context, convID string, depth int) error {
	start := time.Now()
	defer func() {
		dagCondenseDuration.Record(ctx, time.Since(start).Seconds())
	}()

	ctx, span := telemetry.Tracer().Start(ctx, "memory.dag.condense")
	defer span.End()

	if depth+1 >= d.config.MaxDepth {
		return nil
	}

	depthPtr := intPtr(depth)
	nodes, err := d.store.List(ctx, convID, SummaryListOptions{Depth: depthPtr})
	if err != nil {
		return fmt.Errorf("condense: list depth %d: %w", depth, err)
	}

	// Build set of node IDs that already serve as sources for higher-depth nodes,
	// so we don't condense them again.
	nextDepthPtr := intPtr(depth + 1)
	parentNodes, _ := d.store.List(ctx, convID, SummaryListOptions{Depth: nextDepthPtr})
	consumed := make(map[string]bool)
	for _, p := range parentNodes {
		for _, sid := range p.SourceIDs {
			consumed[sid] = true
		}
	}

	// Filter to unconsumed nodes only.
	var unconsumed []*SummaryNode
	for _, n := range nodes {
		if !consumed[n.ID] {
			unconsumed = append(unconsumed, n)
		}
	}

	groupSize := d.config.CondenseGroupSize
	if groupSize <= 1 {
		groupSize = 3
	}

	for i := 0; i+1 < len(unconsumed); i += groupSize {
		end := i + groupSize
		if end > len(unconsumed) {
			end = len(unconsumed)
		}
		group := unconsumed[i:end]
		if len(group) < 2 {
			break
		}

		var combined strings.Builder
		var sourceIDs []string
		earliestSeq := group[0].EarliestSeq
		latestSeq := group[0].LatestSeq
		for _, n := range group {
			fmt.Fprintf(&combined, "[d%d seq %d-%d]\n%s\n\n", n.Depth, n.EarliestSeq, n.LatestSeq, n.Content)
			sourceIDs = append(sourceIDs, n.ID)
			if n.EarliestSeq < earliestSeq {
				earliestSeq = n.EarliestSeq
			}
			if n.LatestSeq > latestSeq {
				latestSeq = n.LatestSeq
			}
		}

		content, expandHint, err := d.summarizeText(ctx, combined.String(), depth+1)
		if err != nil {
			telemetry.Warn(ctx, "dag: condense summarize failed", otellog.String("error", err.Error()))
			continue
		}

		node := &SummaryNode{
			ID:             NewSummaryNodeID(),
			ConversationID: convID,
			Depth:          depth + 1,
			Content:        content,
			ExpandHint:     expandHint,
			SourceIDs:      sourceIDs,
			EarliestSeq:    earliestSeq,
			LatestSeq:      latestSeq,
			TokenCount:     d.counter.Count(content),
			CreatedAt:      time.Now(),
		}

		if err := d.store.Save(ctx, node); err != nil {
			telemetry.Warn(ctx, "dag: save condensed failed", otellog.String("error", err.Error()))
			continue
		}
		dagNodesTotal.Add(ctx, 1)
	}

	// Check if compact is needed after condense.
	allNodes, err := d.store.ListAll(ctx, convID)
	if err == nil && len(allNodes) >= d.config.Compact.CompactThreshold {
		if _, err := d.Compact(ctx, convID); err != nil {
			telemetry.Warn(ctx, "dag: compact after condense failed", otellog.String("error", err.Error()))
		}
	}

	// Recursively check next depth.
	nextDepth := intPtr(depth + 1)
	nextNodes, err := d.store.List(ctx, convID, SummaryListOptions{Depth: nextDepth})
	if err == nil && len(nextNodes) >= d.config.CondenseThreshold {
		return d.condense(ctx, convID, depth+1)
	}

	return nil
}

// Assemble constructs the context window from summaries + recent messages.
func (d *SummaryDAG) Assemble(ctx context.Context, convID string, tokenBudget int) ([]llm.Message, error) {
	start := time.Now()
	defer func() {
		dagAssembleDuration.Record(ctx, time.Since(start).Seconds())
	}()

	ctx, span := telemetry.Tracer().Start(ctx, "memory.dag.assemble")
	defer span.End()

	if tokenBudget <= 0 {
		tokenBudget = d.config.TokenBudget
	}

	msgs, err := d.msgStore.GetMessages(ctx, convID)
	if err != nil {
		return nil, fmt.Errorf("assemble: get messages: %w", err)
	}

	if len(msgs) == 0 {
		return nil, nil
	}

	totalTokens := d.counter.CountMessages(msgs)
	if totalTokens <= tokenBudget {
		return msgs, nil
	}

	// Extract system message.
	var systemMsg *llm.Message
	var nonSystemMsgs []llm.Message
	if len(msgs) > 0 && msgs[0].Role == llm.RoleSystem {
		sys := msgs[0]
		systemMsg = &sys
		nonSystemMsgs = msgs[1:]
	} else {
		nonSystemMsgs = msgs
	}

	systemTokens := 0
	if systemMsg != nil {
		systemTokens = d.counter.Count(systemMsg.Content())
	}

	availableBudget := tokenBudget - systemTokens
	if availableBudget <= 0 {
		availableBudget = tokenBudget / 2
	}

	recentBudget := int(float64(availableBudget) * d.config.RecentRatio)
	midBudget := int(float64(availableBudget) * d.config.MidRatio)
	farBudget := availableBudget - recentBudget - midBudget

	// Recent messages (from tail).
	var recentMsgs []llm.Message
	recentTokens := 0
	recentCutoff := len(nonSystemMsgs)
	for i := len(nonSystemMsgs) - 1; i >= 0; i-- {
		msgTokens := d.countMsg(nonSystemMsgs[i])
		if recentTokens+msgTokens > recentBudget {
			recentCutoff = i + 1
			break
		}
		recentTokens += msgTokens
		if i == 0 {
			recentCutoff = 0
		}
	}
	recentMsgs = nonSystemMsgs[recentCutoff:]

	// Get summaries for earlier messages.
	allSummaries, _ := d.store.List(ctx, convID, SummaryListOptions{})

	var historicalContext strings.Builder
	if len(allSummaries) > 0 && recentCutoff > 0 {
		historicalContext.WriteString("[Historical context]\n\n")

		// Far zone: highest-depth summaries covering earliest messages.
		farTokens := 0
		for _, s := range allSummaries {
			if s.LatestSeq >= recentCutoff {
				continue
			}
			if s.Depth >= 2 && farTokens < farBudget {
				fmt.Fprintf(&historicalContext, "## History (messages %d-%d)\n%s\n",
					s.EarliestSeq, s.LatestSeq, s.Content)
				if s.ExpandHint != "" {
					historicalContext.WriteString(s.ExpandHint + "\n")
				}
				historicalContext.WriteString("\n")
				farTokens += s.TokenCount
			}
		}

		// Mid zone: d0/d1 summaries.
		midTokens := 0
		for _, s := range allSummaries {
			if s.LatestSeq >= recentCutoff {
				continue
			}
			if s.Depth <= 1 && midTokens < midBudget {
				fmt.Fprintf(&historicalContext, "## Recent history (messages %d-%d)\n%s\n",
					s.EarliestSeq, s.LatestSeq, s.Content)
				if s.ExpandHint != "" {
					historicalContext.WriteString(s.ExpandHint + "\n")
				}
				historicalContext.WriteString("\n")
				midTokens += s.TokenCount
			}
		}

		historicalContext.WriteString("[End of historical context]")
	}

	// Assemble result.
	var result []llm.Message
	if systemMsg != nil {
		sysContent := systemMsg.Content()
		hc := historicalContext.String()
		if hc != "" {
			sysContent = sysContent + "\n\n" + hc
		}
		result = append(result, llm.NewTextMessage(llm.RoleSystem, sysContent))
	} else if historicalContext.Len() > 0 {
		result = append(result, llm.NewTextMessage(llm.RoleSystem, historicalContext.String()))
	}

	result = append(result, recentMsgs...)
	return sanitizeToolPairs(result), nil
}

// Compact removes deleted nodes and prunes leaf content.
func (d *SummaryDAG) Compact(ctx context.Context, convID string) (CompactResult, error) {
	start := time.Now()
	defer func() {
		dagCompactDuration.Record(ctx, time.Since(start).Seconds())
	}()

	ctx, span := telemetry.Tracer().Start(ctx, "memory.dag.compact")
	defer span.End()

	allNodes, err := d.store.ListAll(ctx, convID)
	if err != nil {
		return CompactResult{}, fmt.Errorf("compact: list all: %w", err)
	}

	var result CompactResult

	// Build parent map: d0 ID -> has parent (d1 with SourceIDs containing it).
	parentOf := make(map[string]bool)
	for _, n := range allNodes {
		if n.Depth >= 1 && !n.Deleted {
			for _, sid := range n.SourceIDs {
				parentOf[sid] = true
			}
		}
	}

	var kept []*SummaryNode
	for _, n := range allNodes {
		if n.Deleted {
			result.DeletedRemoved++
			continue
		}

		if d.config.Compact.PruneLeafContent && n.Depth == 0 {
			shouldPrune := true
			if d.config.Compact.RequireParent && !parentOf[n.ID] {
				shouldPrune = false
			}
			if shouldPrune && n.Content != "" && !strings.HasPrefix(n.Content, "[pruned") {
				n.Content = "[pruned — use history_expand to load originals]"
				n.TokenCount = 0
				result.LeafPruned++
				dagCompactPruned.Add(ctx, 1)
			}
		}
		kept = append(kept, n)
	}

	result.TotalRemaining = len(kept)

	if err := d.store.Rewrite(ctx, convID, kept); err != nil {
		return result, fmt.Errorf("compact: rewrite: %w", err)
	}

	telemetry.Info(ctx, "dag: compact completed",
		otellog.Int("deleted_removed", result.DeletedRemoved),
		otellog.Int("leaf_pruned", result.LeafPruned),
		otellog.Int("total_remaining", result.TotalRemaining))

	return result, nil
}

func (d *SummaryDAG) summarizeWithFallback(ctx context.Context, msgs []llm.Message, depth int) (content, expandHint string, err error) {
	var b strings.Builder
	for _, msg := range msgs {
		text := msg.Content()
		if text != "" {
			fmt.Fprintf(&b, "%s: %s\n", msg.Role, text)
		}
	}
	return d.summarizeText(ctx, b.String(), depth)
}

func (d *SummaryDAG) summarizeText(ctx context.Context, text string, depth int) (content, expandHint string, err error) {
	prompt := fmt.Sprintf(depthPrompt(depth), text)
	sourceTokens := d.counter.Count(text)

	// L0: Normal.
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	resp, _, genErr := d.llm.Generate(timeoutCtx, []llm.Message{
		llm.NewTextMessage(llm.RoleUser, prompt),
	})
	cancel()

	if genErr == nil {
		content = strings.TrimSpace(resp.Content())
		if content != "" && d.counter.Count(content) <= int(float64(sourceTokens)*0.8) {
			return extractExpandHint(content)
		}
	}

	// L1: Aggressive retry.
	dagFallbackTotal.Add(ctx, 1)
	retryPrompt := fmt.Sprintf("Compress the following to at most %d tokens. Be extremely concise.\n\n%s\n\nCompressed:", sourceTokens/3, text)
	timeoutCtx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	resp2, _, genErr2 := d.llm.Generate(timeoutCtx2, []llm.Message{
		llm.NewTextMessage(llm.RoleUser, retryPrompt),
	})
	cancel2()

	if genErr2 == nil {
		content = strings.TrimSpace(resp2.Content())
		if content != "" {
			return extractExpandHint(content)
		}
	}

	// L2: Deterministic fallback.
	dagFallbackTotal.Add(ctx, 1)
	runes := []rune(text)
	head := string(runes[:min(100, len(runes))])
	tail := ""
	if len(runes) > 100 {
		tailStart := max(100, len(runes)-100)
		tail = string(runes[tailStart:])
	}
	content = fmt.Sprintf("[auto-summary] %s... ...%s", head, tail)
	expandHint = "[LLM summarization failed, use history_expand to see originals]"
	return content, expandHint, nil
}

func extractExpandHint(content string) (string, string, error) {
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "[Expand for details about:") {
			body := strings.Join(lines[:i], "\n")
			return strings.TrimSpace(body), line, nil
		}
	}
	return content, "", nil
}

func (d *SummaryDAG) countMsg(msg llm.Message) int {
	tokens := 4
	for _, p := range msg.Parts {
		switch p.Type {
		case llm.PartText:
			tokens += d.counter.Count(p.Text)
		case llm.PartToolCall:
			if p.ToolCall != nil {
				tokens += d.counter.Count(p.ToolCall.Name) + d.counter.Count(p.ToolCall.Arguments)
			}
		case llm.PartToolResult:
			if p.ToolResult != nil {
				tokens += d.counter.Count(p.ToolResult.Content)
			}
		}
	}
	return tokens
}

func intPtr(i int) *int { return &i }

// ===== SummaryIndex (was compactor_index.go) =====

// BuildSummaryIndex generates a summary index string from the top-level
// summaries of a conversation. The result is intended to be injected into
// the LLM system prompt via the workflow.VarSummaryIndex board variable.
//
// Returns an empty string when no summaries exist or the store is nil.
// The budget parameter controls the maximum character length of the output;
// older summaries are omitted (with a note) when the budget is exceeded.
func BuildSummaryIndex(ctx context.Context, store SummaryStore, convID string, budget int) string {
	if store == nil || convID == "" {
		return ""
	}

	nodes, err := topLevelSummaries(ctx, store, convID)
	if err != nil || len(nodes) == 0 {
		return ""
	}

	return formatSummaryIndex(nodes, budget)
}

// topLevelSummaries returns the highest-depth active summary nodes for a
// conversation, sorted by EarliestSeq ascending.
func topLevelSummaries(ctx context.Context, store SummaryStore, convID string) ([]*SummaryNode, error) {
	all, err := store.ListAll(ctx, convID)
	if err != nil {
		return nil, err
	}

	deleted := make(map[string]bool)
	latest := make(map[string]*SummaryNode)
	maxDepth := 0

	for _, n := range all {
		if n.Deleted {
			deleted[n.ID] = true
			continue
		}
		deleted[n.ID] = false
		latest[n.ID] = n
		if n.Depth > maxDepth {
			maxDepth = n.Depth
		}
	}

	var top []*SummaryNode
	for id, isDel := range deleted {
		if isDel {
			continue
		}
		n := latest[id]
		if n.Depth == maxDepth {
			top = append(top, n)
		}
	}

	sort.Slice(top, func(i, j int) bool {
		return top[i].EarliestSeq < top[j].EarliestSeq
	})

	return top, nil
}

// formatSummaryIndex renders summary nodes into a human-readable index.
// It fills from the newest summary backwards; when the budget is exceeded,
// earlier entries are dropped with a note.
func formatSummaryIndex(nodes []*SummaryNode, budget int) string {
	if budget <= 0 {
		budget = 1500
	}

	const header = "## Conversation Summary\n\nBelow are compressed summaries of earlier conversation. To view the original messages, call history_expand(summary_id=ID).\n\n"

	lines := make([]string, len(nodes))
	for i, n := range nodes {
		content := truncateSummary(n.Content, 200)
		lines[i] = fmt.Sprintf("[%s] seq %d-%d: %s", n.ID, n.EarliestSeq, n.LatestSeq, content)
	}

	remaining := budget - len(header)
	if remaining <= 0 {
		return ""
	}

	var included []string
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		lineLen := len(lines[i]) + 1 // +1 for newline
		if total+lineLen > remaining {
			break
		}
		included = append([]string{lines[i]}, included...)
		total += lineLen
	}

	if len(included) == 0 {
		return ""
	}

	omitted := len(nodes) - len(included)
	var b strings.Builder
	b.WriteString(header)
	if omitted > 0 {
		fmt.Fprintf(&b, "(%d earlier summaries omitted)\n", omitted)
	}
	for _, line := range included {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	return b.String()
}

func truncateSummary(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
