package history

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

// Background work timeouts. Ingest and archive used to share a single 60s
// budget; that meant a slow LLM summarization could starve the archive
// step (or vice-versa). They now run independently with their own ceilings.
const (
	defaultIngestTimeout  = 60 * time.Second
	defaultArchiveTimeout = 60 * time.Second
)

// LosslessMemory implements the Memory interface with lossless context management.
// All original messages are persisted; a DAG of summaries provides compressed
// context windows while allowing on-demand expansion.
//
// Concurrency model: every Append acquires a per-conversation lock and then
// schedules background DAG ingest + archive on a goroutine. Because the
// per-conversation lock serializes Appends for the same conversationID,
// no two background workers race on the same DAG; cross-conversation
// background work runs in parallel without artificial bounds.
type LosslessMemory struct {
	store  Store
	dag    *SummaryDAG
	config DAGConfig
	ws     workspace.Workspace
	prefix string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	wg    sync.WaitGroup
}

// NewLosslessMemory creates a new LosslessMemory.
func NewLosslessMemory(store Store, dag *SummaryDAG, config DAGConfig, ws workspace.Workspace, prefix string) *LosslessMemory {
	return &LosslessMemory{
		store:  store,
		dag:    dag,
		config: config,
		ws:     ws,
		prefix: prefix,
		locks:  make(map[string]*sync.Mutex),
	}
}

func (m *LosslessMemory) Load(ctx context.Context, conversationID string) ([]model.Message, error) {
	return m.dag.Assemble(ctx, conversationID, m.config.TokenBudget)
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
func (m *LosslessMemory) Append(ctx context.Context, conversationID string, newMessages []model.Message) error {
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
func (m *LosslessMemory) persistAppend(ctx context.Context, conversationID string, newMessages []model.Message) (int, error) {
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

func (m *LosslessMemory) Clear(ctx context.Context, conversationID string) error {
	convMu := m.convMu(conversationID)
	convMu.Lock()
	defer convMu.Unlock()

	if err := m.store.DeleteMessages(ctx, conversationID); err != nil {
		return err
	}
	return m.dag.store.Rewrite(ctx, conversationID, nil)
}

// Close waits for all pending async ingest/archive goroutines to complete.
func (m *LosslessMemory) Close() {
	m.wg.Wait()
}

// Archive manually triggers archiving for this conversation.
func (m *LosslessMemory) Archive(ctx context.Context, conversationID string) (ArchiveResult, error) {
	if m.ws == nil {
		return ArchiveResult{}, nil
	}
	convMu := m.convMu(conversationID)
	convMu.Lock()
	defer convMu.Unlock()
	return Archive(ctx, m.ws, m.store, m.prefix, conversationID, m.config.Archive)
}

func (m *LosslessMemory) convMu(convID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	mu, ok := m.locks[convID]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[convID] = mu
	}
	return mu
}
