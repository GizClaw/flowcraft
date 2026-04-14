package memory

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

const defaultMaxIngestConcurrency = 8

// LosslessMemory implements the Memory interface with lossless context management.
// All original messages are persisted; a DAG of summaries provides compressed
// context windows while allowing on-demand expansion.
type LosslessMemory struct {
	store  Store
	dag    *SummaryDAG
	config DAGConfig
	ws     workspace.Workspace
	prefix string
	wg     sync.WaitGroup
	sem    chan struct{}
}

// NewLosslessMemory creates a new LosslessMemory.
func NewLosslessMemory(store Store, dag *SummaryDAG, config DAGConfig, ws workspace.Workspace, prefix string) *LosslessMemory {
	return &LosslessMemory{
		store:  store,
		dag:    dag,
		config: config,
		ws:     ws,
		prefix: prefix,
		sem:    make(chan struct{}, defaultMaxIngestConcurrency),
	}
}

func (m *LosslessMemory) Load(ctx context.Context, conversationID string) ([]model.Message, error) {
	return m.dag.Assemble(ctx, conversationID, m.config.TokenBudget)
}

func (m *LosslessMemory) Save(ctx context.Context, conversationID string, messages []model.Message) error {
	existing, err := m.store.GetMessages(ctx, conversationID)
	prevCount := 0
	if err != nil {
		telemetry.Warn(ctx, "lossless: GetMessages failed, treating as empty",
			otellog.String("error", err.Error()))
	} else {
		prevCount = len(existing)
	}

	if err := m.store.SaveMessages(ctx, conversationID, messages); err != nil {
		return err
	}

	if len(messages) <= prevCount {
		return nil
	}
	newMessages := messages[prevCount:]
	startSeq := prevCount

	// Async DAG ingest + archive, bounded by semaphore.
	select {
	case m.sem <- struct{}{}:
		m.wg.Add(1)
		go func() {
			defer func() { <-m.sem; m.wg.Done() }()
			asyncCtx := context.WithoutCancel(ctx)
			asyncCtx, cancel := context.WithTimeout(asyncCtx, 60*time.Second)
			defer cancel()

			if err := m.dag.Ingest(asyncCtx, conversationID, newMessages, startSeq); err != nil {
				telemetry.Warn(asyncCtx, "lossless: async ingest failed",
					otellog.String("conversation_id", conversationID),
					otellog.String("error", err.Error()))
			}

			if m.ws != nil {
				if _, err := Archive(asyncCtx, m.ws, m.store, m.prefix, conversationID, m.config.Archive); err != nil {
					telemetry.Warn(asyncCtx, "lossless: async archive failed",
						otellog.String("conversation_id", conversationID),
						otellog.String("error", err.Error()))
				}
			}
		}()
	default:
		telemetry.Warn(ctx, "lossless: ingest semaphore full, skipping async ingest",
			otellog.String("conversation_id", conversationID))
	}

	return nil
}

func (m *LosslessMemory) Clear(ctx context.Context, conversationID string) error {
	if err := m.store.DeleteMessages(ctx, conversationID); err != nil {
		return err
	}
	// Clear summaries by rewriting with empty.
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
	return Archive(ctx, m.ws, m.store, m.prefix, conversationID, m.config.Archive)
}
