package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/memory"
)

// StoreSlot is the documented name of one logical store slot.
// The kernel maps these to the six op interfaces:
//
//   - "messages"   — produces Append, Load, Recall
//   - "documents"  — produces Import, Compact, Archive
//
// The slot names are part of the documented schema. A document
// that references an unknown slot is rejected at validate time.
const (
	StoreMessages  = "messages"
	StoreDocuments = "documents"
)

// StoreResult is the partial Impls a StoreFactory produces for
// one named slot. The Builder merges StoreResults across slots
// into the final Impls that the runtime is built with. A
// StoreFactory may leave any of the six op fields nil; the
// runtime treats a nil op as "not configured" and rejects
// calls to it with [memory.KindNotConfigured].
type StoreResult struct {
	Append  memory.AppendOp
	Load    memory.LoadOp
	Recall  memory.RecallOp
	Import  memory.ImportOp
	Compact memory.CompactOp
	Archive memory.ArchiveOp
	// CloseFunc transfers ownership of resources created for this
	// store slot to the Assembly. It is called exactly once.
	CloseFunc memory.CloseFunc
	// Closer is the io.Closer form of CloseFunc. A result must set at
	// most one ownership field.
	Closer io.Closer
}

// StoreInput is the opaque payload a StoreFactory receives for
// one store slot. Settings is the impl-owned JSON subtree the
// factory decodes with whatever strictness its driver requires.
type StoreInput struct {
	// StoreName is the slot the document mapped this entry
	// to: StoreMessages or StoreDocuments.
	StoreName string
	// Impl is the registered factory name (e.g. "sqlite",
	// "noop", "remote"). The Builder only forwards the call
	// when an entry is registered under this name.
	Impl string
	// Settings is the document's settings subtree as
	// json.RawMessage. The factory decodes it into its own
	// typed struct with KnownFields strictness.
	Settings json.RawMessage
	// Inference is the runtime used by document stores to execute Embed.
	// It is nil only when the embedding block is disabled and no runtime
	// dependency was configured.
	Inference *inference.Runtime
	// Embedding is the document's typed embedding policy.
	Embedding EmbeddingSpec
}

// StoreFactory builds a partial Impls for one named store
// slot. The Builder calls the registered factory at NewAssembly
// time; the returned StoreResult is merged into the runtime's
// Impls.
type StoreFactory interface {
	// BuildStore is called once per store slot during
	// NewAssembly. The factory must NOT perform any I/O until
	// the first op call, so a deployment that does not use
	// memory does not pay for opening stores.
	BuildStore(ctx context.Context, in StoreInput) (StoreResult, error)
}

// StoreFactoryFunc is the function-typed adapter for
// StoreFactory.
type StoreFactoryFunc func(ctx context.Context, in StoreInput) (StoreResult, error)

// BuildStore calls f.
func (f StoreFactoryFunc) BuildStore(ctx context.Context, in StoreInput) (StoreResult, error) {
	return f(ctx, in)
}

// Assembly is the resource value the deploy factory returns
// for kind "memory.Assembly". It is the single shared object
// every agent Instance / Run pulls from: Runtime is the kernel
// handle, Embedding is the inference config the documents store
// holds internally, Lifecycle is the scheduler-side config the
// independent scheduler reads.
type Assembly struct {
	// Runtime is the kernel handle every memory op runs
	// against. Close releases it; assembly.Close defers here.
	Runtime *memory.Runtime
	// Spec is the kernel Spec the Runtime was built with.
	// Hooks / tool factories can read DefaultScope off it
	// without going through the Builder.
	Spec memory.Spec
	// Embedding is the document's embedding block, kept here
	// so document-store impls (or diagnostics) can read it
	// without re-decoding the document.
	Embedding EmbeddingSpec
	// Lifecycle is the document's lifecycle block, kept here
	// so the scheduler can read it without re-decoding the
	// document.
	Lifecycle LifecycleSpec
	// Document is the source-of-truth. Callers that need to
	// re-render the document (e.g. for diagnostics) read
	// this. It is the immutable snapshot the assembly was
	// built from.
	Document Document
}

// Close releases the runtime. It is idempotent and is what
// the deploy framework calls when shutting the build down.
func (a *Assembly) Close() error {
	if a == nil || a.Runtime == nil {
		return nil
	}
	return a.Runtime.Close()
}

// ResolveItem exposes the runtime to deploy hook dependencies as
// "memory/runtime". The assembly remains the deploy-owned closer.
func (a *Assembly) ResolveItem(ref string) (any, bool) {
	if a == nil || ref != "runtime" || a.Runtime == nil {
		return nil, false
	}
	return a.Runtime, true
}

// Builder turns a Document into an Assembly. It is immutable
// after construction and safe for concurrent use when its
// factories are safe for concurrent use. The package does not
// expose package-global state: every host that needs memory
// builds its own Builder.
type Builder struct {
	factories map[string]StoreFactory
}

// NewBuilder validates the factory catalog and returns a
// ready-to-use Builder. An empty catalog is allowed: a deployment
// that only references noop stores still works.
func NewBuilder(factories map[string]StoreFactory) (*Builder, error) {
	out := &Builder{factories: make(map[string]StoreFactory, len(factories))}
	for name, factory := range factories {
		if !identifierPattern.MatchString(name) {
			return nil, fmt.Errorf("memory config: invalid factory name %q", name)
		}
		if factory == nil {
			return nil, fmt.Errorf("memory config: factory %q is nil", name)
		}
		out.factories[name] = factory
	}
	return out, nil
}

// NewAssembly builds the runtime plus the sidecar configuration used by the
// scheduler and document stores. The optional "inference" dependency is
// required whenever embedding is enabled.
func (b *Builder) NewAssembly(
	ctx context.Context,
	doc Document,
	deps map[string]any,
) (*Assembly, error) {
	if err := doc.Validate(); err != nil {
		return nil, errdefs.Validation(fmt.Errorf("memory config: %w", err))
	}
	inferenceRuntime, err := resolveInferenceRuntime(doc.Embedding, deps)
	if err != nil {
		return nil, err
	}
	if err := validateEmbeddingModel(doc.Embedding, inferenceRuntime); err != nil {
		return nil, err
	}

	clock, err := buildClock(doc.Runtime.Clock)
	if err != nil {
		return nil, errdefs.Validation(err)
	}

	kernelSpec := memory.Spec{
		RuntimeID:    doc.Runtime.DefaultScope.RuntimeID,
		DefaultScope: kernelScopeFromSpec(doc.Runtime.DefaultScope),
		Clock:        clock,
	}

	impls, closers, err := b.buildImpls(ctx, doc, inferenceRuntime)
	if err != nil {
		return nil, err
	}

	rt, err := memory.New(kernelSpec, impls)
	if err != nil {
		return nil, errors.Join(
			errdefs.Validation(fmt.Errorf("memory config: %w", err)),
			closeStores(closers),
		)
	}
	for _, close := range closers {
		rt.RegisterClose(close)
	}

	return &Assembly{
		Runtime:   rt,
		Spec:      kernelSpec,
		Embedding: doc.Embedding,
		Lifecycle: doc.Lifecycle,
		Document:  doc,
	}, nil
}

// buildImpls walks the document's store slots, calls the
// matching StoreFactory for each, and merges the results into
// a single memory.Impls. A document that omits a slot
// silently leaves the corresponding op nil.
func (b *Builder) buildImpls(
	ctx context.Context,
	doc Document,
	inferenceRuntime *inference.Runtime,
) (memory.Impls, []memory.CloseFunc, error) {
	var out memory.Impls
	var closers []memory.CloseFunc
	owners := map[string]string{}
	names := make([]string, 0, len(doc.Stores))
	for name := range doc.Stores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := doc.Stores[name]

		factory, ok := b.factories[entry.Impl]
		if !ok {
			return out, closers, errors.Join(fmt.Errorf(
				"memory config: stores[%q].impl %q is not registered", name, entry.Impl),
				closeStores(closers))
		}
		result, err := factory.BuildStore(ctx, StoreInput{
			StoreName: name,
			Impl:      entry.Impl,
			Settings:  entry.Settings,
			Inference: inferenceRuntime,
			Embedding: doc.Embedding,
		})
		if err != nil {
			return out, closers, errors.Join(fmt.Errorf(
				"memory config: build stores[%q] (impl %q): %w", name, entry.Impl, err),
				closeStores(closers))
		}
		close, err := storeClose(result)
		if err != nil {
			if close != nil {
				closers = append(closers, close)
			}
			return out, closers, errors.Join(fmt.Errorf(
				"memory config: stores[%q]: %w", name, err), closeStores(closers))
		}
		if close != nil {
			closers = append(closers, close)
		}
		if err := mergeInto(&out, owners, name, result); err != nil {
			return out, closers, errors.Join(err, closeStores(closers))
		}
	}
	return out, closers, nil
}

func resolveInferenceRuntime(
	embedding EmbeddingSpec,
	deps map[string]any,
) (*inference.Runtime, error) {
	value, configured := deps["inference"]
	if !configured {
		if embedding.Enabled() {
			return nil, errdefs.Validationf(
				"memory config: embedding requires inference dependency")
		}
		return nil, nil
	}
	runtime, ok := value.(*inference.Runtime)
	if !ok || runtime == nil {
		return nil, errdefs.Validationf(
			"memory config: dependency %q has Go type %T, want *inference.Runtime",
			"inference", value)
	}
	return runtime, nil
}

func validateEmbeddingModel(
	embedding EmbeddingSpec,
	runtime *inference.Runtime,
) error {
	if !embedding.Enabled() {
		return nil
	}
	descriptor, err := runtime.InspectModel(embedding.Model)
	if err != nil {
		return errdefs.Validation(fmt.Errorf(
			"memory config: embedding.model cannot be resolved: %w", err))
	}
	for _, operation := range descriptor.Operations {
		if operation == inference.OperationEmbed {
			return nil
		}
	}
	return errdefs.Validationf(
		"memory config: embedding.model does not support operation %q",
		inference.OperationEmbed)
}

// mergeInto folds a StoreResult into a running Impls. The
// first writer owns each operation; a later slot that provides
// the same operation causes a validation error.
func mergeInto(into *memory.Impls, owners map[string]string, slot string, from StoreResult) error {
	type binding struct {
		name string
		set  func()
		used bool
	}
	bindings := []binding{
		{"append", func() { into.Append = from.Append }, from.Append != nil},
		{"load", func() { into.Load = from.Load }, from.Load != nil},
		{"recall", func() { into.Recall = from.Recall }, from.Recall != nil},
		{"import", func() { into.Import = from.Import }, from.Import != nil},
		{"compact", func() { into.Compact = from.Compact }, from.Compact != nil},
		{"archive", func() { into.Archive = from.Archive }, from.Archive != nil},
	}
	for _, binding := range bindings {
		if !binding.used {
			continue
		}
		if first, exists := owners[binding.name]; exists {
			return errdefs.Validationf(
				"memory config: stores[%q] and stores[%q] both provide %s",
				first, slot, binding.name)
		}
		owners[binding.name] = slot
		binding.set()
	}
	return nil
}

func storeClose(result StoreResult) (memory.CloseFunc, error) {
	if result.CloseFunc != nil && result.Closer != nil {
		return func() error {
			return errors.Join(result.CloseFunc(), result.Closer.Close())
		}, errors.New("StoreResult must not set both CloseFunc and Closer")
	}
	if result.CloseFunc != nil {
		return result.CloseFunc, nil
	}
	if result.Closer != nil {
		return result.Closer.Close, nil
	}
	return nil, nil
}

func closeStores(closers []memory.CloseFunc) error {
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildClock resolves the document's clock.impl to a concrete
// memory.Clock. Only "system" and "frozen" are built-in; the
// Builder keeps the mapping in one place so a host can
// subclass by wrapping.
func buildClock(spec ClockSpec) (memory.Clock, error) {
	switch spec.Impl {
	case "system", "":
		return memory.SystemClock, nil
	case "frozen":
		return frozenClock{}, nil
	default:
		return nil, fmt.Errorf("memory config: unknown clock impl %q", spec.Impl)
	}
}

// kernelScopeFromSpec maps the YAML ScopeSpec into the
// kernel's [memory.Scope]. The kernel's RuntimeID and UserID
// are required / optional respectively; the validator already
// rejected an empty RuntimeID, so this is a straight copy.
func kernelScopeFromSpec(s ScopeSpec) memory.Scope {
	return memory.Scope{
		RuntimeID: s.RuntimeID,
		UserID:    s.UserID,
	}
}

// frozenClock is a deterministic Clock for tests. It always
// returns the zero time so behaviour does not depend on the
// wall clock during replay.
type frozenClock struct{}

// Now returns the zero time.
func (frozenClock) Now() time.Time { return time.Time{} }
