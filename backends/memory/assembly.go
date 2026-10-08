package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval"
	"github.com/GizClaw/flowcraft/backends/memory/sources"
	docsource "github.com/GizClaw/flowcraft/backends/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/verify"
	docview "github.com/GizClaw/flowcraft/backends/memory/views/document"
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
	docViews  *docview.DocumentViewStore
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
	// lifecycleMu guards closed and the cancel/done pair it publishes, so a
	// concurrent Wire and Close cannot race on them and Close-before-Wire
	// cannot be followed by a runner started over closed pools.
	lifecycleMu sync.Mutex
	closed      bool
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
	docViews *docview.DocumentViewStore,
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
	if docViews == nil {
		return nil, errors.New("document view is required")
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
		messages: messages, documents: documents, docViews: docViews,
		facts: facts, summaries: summaries,
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
		assembly.lifecycleMu.Lock()
		defer assembly.lifecycleMu.Unlock()
		if assembly.closed {
			assembly.wireErr = errors.New("memory assembly: closed")
			return
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

// Close stops the derivation runner and releases the pools the assembly opened.
// The workspace and inference dependencies are borrowed and are not closed
// here. Close is idempotent, and a Wire after Close fails instead of starting a
// runner over closed pools.
func (assembly *Assembly) Close() error {
	if assembly == nil {
		return nil
	}
	var failures []error
	assembly.lifecycleMu.Lock()
	if assembly.closed {
		assembly.lifecycleMu.Unlock()
		return nil
	}
	assembly.closed = true
	cancel, done := assembly.cancel, assembly.done
	assembly.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
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
			// the unchanged watermark. Record the error as well: the
			// processor stores its own derivation failures, but an early
			// failure such as listing the scope catalog would otherwise be
			// dropped here and never reach Diagnostics.
			if err := assembly.RunOnce(ctx); err != nil && assembly.processor != nil {
				assembly.processor.RecordError(err)
			}
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

// RecentSettings returns the recent-message window this assembly serves with,
// with the library defaults applied. A request may narrow it (RecentLimit,
// RecentMaxTokens, or the pack budget), but this is the configured value: hosts
// report it so a run records the window it measured under rather than the flag
// it passed, which a settings document can override.
func (assembly *Assembly) RecentSettings() RecentSettings {
	recent := RecentSettings{}
	if assembly != nil {
		recent = assembly.recent
	}
	if recent.MaxItems <= 0 {
		recent.MaxItems = defaultRecentMaxItems
	}
	if recent.MaxTokens <= 0 {
		recent.MaxTokens = defaultRecentMaxTokens
	}
	return recent
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

// DocumentViews returns the derived document chunk view. Unlike Facts and
// Summaries it is not generation scoped -- a chunk is addressed by the document
// provenance and text it was derived from, so a policy change reproduces the
// build it produced -- and its superseded builds are reclaimed by
// Assembly.Maintain or by DocumentViewStore.RetireScopeBuilds directly.
func (assembly *Assembly) DocumentViews() *docview.DocumentViewStore {
	if assembly == nil {
		return nil
	}
	return assembly.docViews
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

// DerivationState reports the derivation this assembly's worker owes one
// conversation: the state RetireGenerations refuses to sweep beside. It is the
// observable half of that guard, for a host that would rather sweep when the
// conversation is settled than retry a refused sweep.
func (assembly *Assembly) DerivationState(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
) (worker.DerivationState, error) {
	if assembly == nil || assembly.processor == nil {
		return worker.DerivationState{}, errors.New("memory assembly: derivation worker is not configured")
	}
	if ctx == nil {
		return worker.DerivationState{}, errors.New("memory assembly: context is required")
	}
	return assembly.processor.DerivationState(ctx, scope, conversationID)
}

// RetireResult reports what one retention sweep removed.
type RetireResult struct {
	// Facts counts the stored fact records of the retired generations.
	Facts int `json:"facts"`
	// Summaries counts the retired manifest bookmarks. Summary records are
	// content addressed and shared between generations, so none are removed.
	Summaries int `json:"summaries"`
	// LaneEntries counts the entries the sweep dropped from the lanes: one
	// address per fact of a retired generation that the generation readers
	// resolve does not share, leaving each configured lane.
	LaneEntries int `json:"lane_entries"`
	// Watermarks counts the derivation cursors the sweep retired with the
	// generations they belong to: one per retired generation this worker had
	// derived under, on the message stream of the conversation. Retiring a
	// cursor is what makes a later rollback to that policy re-derive the
	// conversation instead of resuming after the facts the sweep removed.
	Watermarks int `json:"watermarks"`
}

// RetireGenerations runs one host-invoked retention sweep over a conversation:
// every derived generation a retention policy no longer keeps leaves the store
// in one call -- the facts, the lane entries, the summary bookmarks, and the
// derivation progress it would otherwise resume from. Whatever each view is
// serving stays -- reads have to resolve somewhere -- and the caller names the
// rest to keep, exactly as the views' own sweeps do.
//
// One call, because the views share the generation identity: retiring the facts
// of a generation while its summary manifest stays bookmarked leaves a rollback
// that serves summaries of facts that are gone. The sweep therefore keeps every
// generation a view is serving, so a policy change that has left the two views
// on different generations is not pruned while they disagree, and it retires
// the summary bookmarks before the facts: a sweep interrupted between the two
// leaves unreachable facts, which the next sweep collects, instead of a
// bookmark to facts that no longer exist.
//
// Derivation progress is retired with the generations it belongs to, before
// anything else: a watermark is where the next pass under that policy resumes,
// so a generation whose facts are retired while its cursor stays at the end of
// the stream is one a later rollback re-derives only after the cursor -- the
// commits it already covered are never derived again, and readers resolve a
// generation missing their facts. Dropping the cursor instead makes that
// rollback derive the generation again, from the canonical commits, which the
// sweep left where they were. Progress goes first because it is the one step
// that removes a pointer: an interrupted sweep then leaves facts without a
// cursor, which the next pass reproduces, rather than facts retired under a
// cursor that still points past them.
//
// The projection lanes are swept next, and for a reason of their own: an entry
// is addressed by the fact id it projects, so once a generation's facts are gone
// no pass can enumerate the entries it left behind -- the converge walks the
// stored generations -- and the lane would keep offering a candidate the read
// path cannot hydrate. A generation whose pass failed after projecting is the
// case that reaches it: its facts are stored and projected, but it never
// published, so nothing reconciles the lanes with it.
//
// Sweeping is a maintenance action, and it expects derivation over the
// conversation to be quiesced, which the sweep enforces rather than assumes:
// the generations it purges are listed before they are retired, so a pass
// writing one mid-sweep can be published with the facts and the entries the
// sweep removed under it, and derivation resumes after its own watermark -- the
// commits that generation already covered are never derived again. A sweep
// therefore refuses (errors.Is(err, worker.ErrDerivationUnsettled)) while a pass
// over the conversation is running, or while this worker has derived its own
// generation without publishing it. Both are retryable, and both mean the sweep
// retired nothing: derive again -- RunOnce is synchronous -- and sweep after the
// pass rather than beside it.
//
// The sweep is per conversation, over the views a conversation's derivation
// fills. Derived documents are not among them: a document chunk is addressed by
// the document it came from, not by a policy, so the view keeps no generation
// and a superseded build is reclaimed at scope granularity by
// Assembly.Maintain (see DocumentViewStore.RetireScopeBuilds).
func (assembly *Assembly) RetireGenerations(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	keep ...string,
) (RetireResult, error) {
	if assembly == nil || assembly.facts == nil {
		return RetireResult{}, errors.New("memory assembly: fact view is not configured")
	}
	if ctx == nil {
		return RetireResult{}, errors.New("memory assembly: context is required")
	}
	if assembly.processor != nil {
		if err := assembly.processor.RequireSettled(ctx, scope, conversationID); err != nil {
			return RetireResult{}, fmt.Errorf("memory assembly: retention sweep refused: %w", err)
		}
	}
	retained := append([]string(nil), keep...)
	if active, found, err := assembly.facts.ActiveGeneration(ctx, scope, conversationID); err != nil {
		return RetireResult{}, err
	} else if found {
		retained = append(retained, active)
	}
	if assembly.summaries != nil {
		if manifest, found, err := assembly.summaries.LoadActive(ctx, scope, conversationID); err != nil {
			return RetireResult{}, err
		} else if found {
			retained = append(retained, manifest.GenerationID)
		}
	}
	result := RetireResult{}
	// The worker's steps come first, and the progress before the lanes.
	if assembly.processor != nil {
		generations, err := assembly.facts.ListGenerations(ctx, scope, conversationID)
		if err != nil {
			return result, err
		}
		kept := make(map[string]struct{}, len(retained))
		for _, generation := range retained {
			kept[generation] = struct{}{}
		}
		var retiring []string
		for _, generation := range generations {
			if _, ok := kept[generation]; !ok {
				retiring = append(retiring, generation)
			}
		}
		// Derivation progress goes first. It is a pointer, not derived state:
		// dropping it changes what the next pass under that policy derives, not
		// what any reader resolves, and a sweep interrupted here leaves the
		// generation's facts with no cursor -- which re-deriving under that
		// policy reproduces from the canonical commits (the addresses are
		// content derived). Retiring it after the facts would leave the window
		// this ordering exists to close: the facts gone while the cursor still
		// points past them.
		watermarks, err := assembly.processor.RetireProgress(ctx, scope, conversationID, retiring)
		if err != nil {
			return result, err
		}
		result.Watermarks = watermarks
		// The lanes go next: what is about to be retired is still enumerable
		// now, and never again afterwards.
		entries, err := assembly.processor.PurgeGenerations(ctx, scope, conversationID, retiring)
		if err != nil {
			return result, err
		}
		result.LaneEntries = entries
	}
	if assembly.summaries != nil {
		removed, err := assembly.summaries.RetireGenerations(ctx, scope, conversationID, retained...)
		if err != nil {
			return result, err
		}
		result.Summaries = removed
	}
	removed, err := assembly.facts.RetireGenerations(ctx, scope, conversationID, retained...)
	result.Facts = removed
	return result, err
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
		IdempotencyKey: turn.IdempotencyKey, Messages: turn.Messages,
		MessageMetadata: turn.MessageMetadata, Metadata: turn.Metadata,
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
