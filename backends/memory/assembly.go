package memory

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval"
	"github.com/GizClaw/flowcraft/backends/memory/sources"
	docsource "github.com/GizClaw/flowcraft/backends/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/verify"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	"github.com/GizClaw/flowcraft/backends/memory/worker"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/resource"
)

// Assembly implements the core/memory capability contracts on top of an
// append-only Log, a KV store, a scope catalog, derived views, projection
// lanes, and the derivation worker.
type Assembly struct {
	messages  *msgsource.MessageStore
	documents *docsource.DocumentStore
	facts     *factview.FactStore
	summaries *summaryview.SummaryStore
	catalog   *sources.ScopeCatalog
	scopes    []corememory.Scope
	recent    RecentSettings
	provider  *retrieval.Provider
	processor *worker.Processor
	maintain  *maintain.Service
	interval  time.Duration
	clock     func() time.Time
	metrics   *assemblyMetrics
	closers   []io.Closer
	verify    func(context.Context, corememory.Scope, string) (verify.Plan, error)

	wireOnce sync.Once
	wireErr  error
	cancel   context.CancelFunc
	done     chan struct{}
}

var (
	_ corememory.Assembly        = (*Assembly)(nil)
	_ resource.Wireable          = (*Assembly)(nil)
	_ resource.ItemResolver      = (*Assembly)(nil)
	_ interface{ Close() error } = (*Assembly)(nil)
)

func newAssembly(
	messages *msgsource.MessageStore,
	documents *docsource.DocumentStore,
	facts *factview.FactStore,
	summaries *summaryview.SummaryStore,
	catalog *sources.ScopeCatalog,
	scopes []corememory.Scope,
	recent RecentSettings,
	provider *retrieval.Provider,
	processor *worker.Processor,
	maintainService *maintain.Service,
	interval time.Duration,
	clock func() time.Time,
	closers []io.Closer,
	verify func(context.Context, corememory.Scope, string) (verify.Plan, error),
) (*Assembly, error) {
	if messages == nil {
		return nil, errors.New("message store is required")
	}
	if documents == nil {
		return nil, errors.New("document store is required")
	}
	if facts == nil {
		return nil, errors.New("fact view is required")
	}
	if catalog == nil {
		return nil, errors.New("scope catalog is required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &Assembly{
		messages: messages, documents: documents, facts: facts, summaries: summaries,
		catalog: catalog,
		scopes:  append([]corememory.Scope(nil), scopes...),
		recent:  recent, provider: provider, processor: processor,
		maintain: maintainService,
		interval: interval, clock: clock,
		metrics: newAssemblyMetrics(),
		closers: append([]io.Closer(nil), closers...),
		verify:  verify,
	}, nil
}

// Wire registers the configured scope seeds and starts the derivation runner
// when an interval is configured. Registration is idempotent.
func (assembly *Assembly) Wire(ctx context.Context) error {
	if assembly == nil {
		return errors.New("memory assembly: assembly is required")
	}
	assembly.wireOnce.Do(func() {
		for _, scope := range assembly.scopes {
			if err := assembly.catalog.Register(ctx, scope); err != nil {
				assembly.wireErr = err
				return
			}
		}
		if assembly.processor == nil || assembly.interval <= 0 {
			// The derivation loop is optional.
		} else {
			runCtx, cancel := context.WithCancel(context.Background())
			assembly.cancel = cancel
			assembly.done = make(chan struct{})
			go assembly.runLoop(runCtx)
		}
	})
	return assembly.wireErr
}

// Close stops the derivation runner. The workspace and inference dependencies
// are borrowed and are not closed here.
func (assembly *Assembly) Close() error {
	if assembly == nil {
		return nil
	}
	var failures []error
	if assembly.cancel != nil {
		assembly.cancel()
		<-assembly.done
	}
	for index := len(assembly.closers) - 1; index >= 0; index-- {
		if err := assembly.closers[index].Close(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// RunOnce scans every registered scope exactly once. It is the synchronous
// entry point behind the background runner and is safe to call from tests.
func (assembly *Assembly) RunOnce(ctx context.Context) error {
	if assembly == nil || assembly.processor == nil {
		return nil
	}
	if ctx == nil {
		return corememory.NewError(corememory.KindInvalidRequest, "worker", errors.New("context is required"))
	}
	scopes, err := assembly.catalog.List(ctx)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		if err := assembly.processor.ProcessScope(ctx, scope); err != nil {
			return err
		}
	}
	return nil
}

func (assembly *Assembly) runLoop(ctx context.Context) {
	defer close(assembly.done)
	ticker := time.NewTicker(assembly.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Failures stay in the canonical log; the next tick retries from
			// the unchanged watermark. The error itself is available through
			// Diagnostics and the runner's LastError.
			_ = assembly.RunOnce(ctx)
		}
	}
}

// ResolveItem implements resource.ItemResolver: "system" exposes the
// capability adapter.
func (assembly *Assembly) ResolveItem(ref string) (any, bool) {
	if assembly == nil || ref != "system" {
		return nil, false
	}
	return assembly, true
}

// ContextStageTotals reports cumulative per-phase retrieval latency. It exists
// so a benchmark can attribute time inside Context() under concurrency.
func (assembly *Assembly) ContextStageTotals() map[string]StageStat {
	if assembly == nil || assembly.provider == nil {
		return nil
	}
	return assembly.provider.StageTotals()
}

// StageStat re-exports the retrieval stage timing type.
type StageStat = retrieval.StageStat

// PolicyDigest returns the derivation policy digest this assembly was built
// with. Stored projections and watermarks are keyed by it, so hosts use it to
// detect that a workspace was written by a different policy (and to stamp eval
// results). It is empty for an assembly with no derivation worker.
func (assembly *Assembly) PolicyDigest() string {
	if assembly == nil || assembly.processor == nil {
		return ""
	}
	return assembly.processor.PolicyDigest()
}

// MessageStore returns the canonical message store.
func (assembly *Assembly) MessageStore() *msgsource.MessageStore {
	if assembly == nil {
		return nil
	}
	return assembly.messages
}

// DocumentStore returns the canonical document store.
func (assembly *Assembly) DocumentStore() *docsource.DocumentStore {
	if assembly == nil {
		return nil
	}
	return assembly.documents
}

// Facts returns the derived fact view.
func (assembly *Assembly) Facts() *factview.FactStore {
	if assembly == nil {
		return nil
	}
	return assembly.facts
}

// Summaries returns the derived summary view, or nil when summaries are
// disabled.
func (assembly *Assembly) Summaries() *summaryview.SummaryStore {
	if assembly == nil {
		return nil
	}
	return assembly.summaries
}

// Verify runs the read-only integrity checks for one conversation: dangling
// fact links, source digest drift, summary digests, and projection digest
// drift. It never mutates state; repair execution is host-owned.
func (assembly *Assembly) Verify(ctx context.Context, scope corememory.Scope, conversationID string) (verify.Plan, error) {
	if assembly == nil || assembly.verify == nil {
		return verify.Plan{}, errors.New("memory assembly: verifier is not configured")
	}
	if ctx == nil {
		return verify.Plan{}, errors.New("memory assembly: context is required")
	}
	return assembly.verify(ctx, scope, conversationID)
}

// Maintain runs one host-invoked maintenance pass for a scope: it detects
// superseded and aged facts, persists the read-path overlay, and returns the
// plan. Canonical facts are never edited; the overlay only decays retrieval
// scores. Hosts schedule this however they want (there is no in-process
// runner).
func (assembly *Assembly) Maintain(ctx context.Context, scope corememory.Scope) (maintain.Plan, error) {
	if assembly == nil || assembly.maintain == nil {
		return maintain.Plan{}, errors.New("memory assembly: maintenance is not configured")
	}
	if ctx == nil {
		return maintain.Plan{}, errors.New("memory assembly: context is required")
	}
	return assembly.maintain.RunScope(ctx, scope)
}

// Catalog returns the scope catalog.
func (assembly *Assembly) Catalog() *sources.ScopeCatalog {
	if assembly == nil {
		return nil
	}
	return assembly.catalog
}

// Context implements core/memory.ContextProvider. The hybrid provider serves
// the recent lane plus the configured projection lanes; the recent-only path
// remains as a fallback when no provider was built.
func (assembly *Assembly) Context(ctx context.Context, request corememory.ContextRequest) (corememory.ContextResult, error) {
	if assembly == nil {
		return corememory.ContextResult{}, corememory.NewError(
			corememory.KindNotConfigured, "context", errors.New("memory assembly is incomplete"))
	}
	if assembly.provider != nil {
		result, err := assembly.provider.Context(ctx, request)
		if err != nil {
			return corememory.ContextResult{}, err
		}
		assembly.metrics.contextServed(ctx, len(result.Items))
		return result, nil
	}
	result, err := assembly.contextRecent(ctx, request)
	if err != nil {
		return corememory.ContextResult{}, err
	}
	assembly.metrics.contextServed(ctx, len(result.Items))
	return result, nil
}

// CommitTurn implements core/memory.TurnSink.
func (assembly *Assembly) CommitTurn(ctx context.Context, turn corememory.Turn) error {
	if assembly == nil || assembly.messages == nil {
		return corememory.NewError(corememory.KindNotConfigured, "turn", errors.New("memory assembly is incomplete"))
	}
	if ctx == nil {
		return corememory.NewError(corememory.KindInvalidRequest, "turn", errors.New("context is required"))
	}
	if err := turn.Validate(); err != nil {
		return err
	}
	turn = turn.Clone()
	if err := assembly.catalog.Register(ctx, turn.Scope); err != nil {
		return classify(err, "turn", corememory.KindProviderFailure)
	}
	_, err := assembly.messages.Commit(ctx, msgsource.AppendRequest{
		Scope: turn.Scope, ConversationID: turn.ConversationID,
		IdempotencyKey: turn.IdempotencyKey, Messages: turn.Messages, Metadata: turn.Metadata,
	})
	return classify(err, "turn", corememory.KindProviderFailure)
}

// PutDocument implements core/memory.DocumentSink.
func (assembly *Assembly) PutDocument(ctx context.Context, document corememory.Document) error {
	if assembly == nil || assembly.documents == nil {
		return corememory.NewError(corememory.KindNotConfigured, "document", errors.New("memory assembly is incomplete"))
	}
	if ctx == nil {
		return corememory.NewError(corememory.KindInvalidRequest, "document", errors.New("context is required"))
	}
	if err := document.Validate(); err != nil {
		return err
	}
	document = document.Clone()
	if err := assembly.catalog.Register(ctx, document.Scope); err != nil {
		return classify(err, "document", corememory.KindProviderFailure)
	}
	_, err := assembly.documents.Put(ctx, docsource.PutRequest{
		Scope: document.Scope, DatasetID: document.DatasetID, DocumentID: document.DocumentID,
		IdempotencyKey: document.IdempotencyKey, Content: document.Content,
		Provenance: document.Provenance, Metadata: document.Metadata,
	})
	return classify(err, "document", corememory.KindProviderFailure)
}

func classify(err error, capability string, fallback corememory.ErrorKind) error {
	if err == nil {
		return nil
	}
	if corememory.AsError(err) != nil {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return corememory.NewError(corememory.KindOperationInterrupted, capability, err)
	}
	return corememory.NewError(fallback, capability, err)
}
