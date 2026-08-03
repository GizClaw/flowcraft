package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

// NoopStoreFactory returns a StoreResult that satisfies every
// op with a [memory.NoopRuntime]-backed implementation. It is
// the default factory for tests and for deployments that
// reference a "noop" store impl in their memory.yaml.
type NoopStoreFactory struct{}

// BuildStore returns a StoreResult that always satisfies the
// runtime's compile ledger and returns zero values. The same
// noop instance is reused across slots: NoopRuntime is
// goroutine-safe and stateless beyond the runtime's close
// tracking.
func (NoopStoreFactory) BuildStore(ctx context.Context, in StoreInput) (StoreResult, error) {
	// The runtime owns a single NoopRuntime; here we just
	// stamp the noop's op interfaces on whichever slots the
	// document asked for. BuildStore is called once per
	// slot, so the easiest path is to construct a fresh
	// NoopRuntime-shaped adapter for every slot.
	//
	// To keep things small, we reuse one noop across all
	// slots but assign the right op to the right field based
	// on the slot name. This avoids running BuildStore per
	// slot; BuildStore is still called once per slot because
	// the document's stores map dictates that, but each call
	// does not open any I/O.
	noop := noopOps{}
	switch in.StoreName {
	case StoreMessages:
		return StoreResult{Append: noop, Load: noop, Recall: noop}, nil
	case StoreDocuments:
		return StoreResult{Import: noop, Compact: noop, Archive: noop}, nil
	default:
		return StoreResult{}, nil
	}
}

// noopOps is a private adapter that turns NoopRuntime's per-op
// methods into a single value satisfying every op interface.
// The methods are no-ops; tests cover the round-trip in
// sdk/memory/memorytest.
type noopOps struct{}

func (noopOps) CompileAppend(_ context.Context, _ memory.AppendRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpAppend,
		Decisions: []memory.Decision{
			{Field: memory.FieldAppendScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldAppendConversationID, Disposition: memory.DispositionNative},
			{Field: memory.FieldAppendIdempotencyKey, Disposition: memory.DispositionNative},
			{Field: memory.FieldAppendRecords, Disposition: memory.DispositionNative},
			{Field: memory.FieldAppendMetadata, Disposition: memory.DispositionNative},
		},
	}
}
func (noopOps) ExecuteAppend(_ context.Context, _ memory.AppendRequest) (memory.AppendResponse, error) {
	return memory.AppendResponse{}, nil
}

func (noopOps) CompileLoad(_ context.Context, _ memory.LoadRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpLoad,
		Decisions: []memory.Decision{
			{Field: memory.FieldLoadScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldLoadConversationID, Disposition: memory.DispositionNative},
			{Field: memory.FieldLoadCursor, Disposition: memory.DispositionNative},
			{Field: memory.FieldLoadLimit, Disposition: memory.DispositionNative},
			{Field: memory.FieldLoadReverse, Disposition: memory.DispositionNative},
		},
	}
}
func (noopOps) ExecuteLoad(_ context.Context, _ memory.LoadRequest) (memory.LoadResponse, error) {
	return memory.LoadResponse{}, nil
}

func (noopOps) CompileRecall(_ context.Context, _ memory.RecallRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpRecall,
		Decisions: []memory.Decision{
			{Field: memory.FieldRecallScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallConversationID, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallQuery, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallTopK, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallFilters, Disposition: memory.DispositionNative},
			{Field: memory.FieldRecallMinScore, Disposition: memory.DispositionNative},
		},
	}
}
func (noopOps) ExecuteRecall(_ context.Context, _ memory.RecallRequest) (memory.RecallResponse, error) {
	return memory.RecallResponse{}, nil
}

func (noopOps) CompileImport(_ context.Context, _ memory.ImportRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpImport,
		Decisions: []memory.Decision{
			{Field: memory.FieldImportScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldImportDatasetID, Disposition: memory.DispositionNative},
			{Field: memory.FieldImportSource, Disposition: memory.DispositionNative},
			{Field: memory.FieldImportTags, Disposition: memory.DispositionNative},
			{Field: memory.FieldImportChunkPolicy, Disposition: memory.DispositionNative},
		},
	}
}
func (noopOps) ExecuteImport(_ context.Context, _ memory.ImportRequest) (memory.ImportResponse, error) {
	return memory.ImportResponse{}, nil
}

func (noopOps) CompileCompact(_ context.Context, _ memory.CompactRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpCompact,
		Decisions: []memory.Decision{
			{Field: memory.FieldCompactScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldCompactOlderThan, Disposition: memory.DispositionNative},
			{Field: memory.FieldCompactKeep, Disposition: memory.DispositionNative},
		},
	}
}
func (noopOps) ExecuteCompact(_ context.Context, _ memory.CompactRequest) (memory.CompactResponse, error) {
	return memory.CompactResponse{}, nil
}

func (noopOps) CompileArchive(_ context.Context, _ memory.ArchiveRequest) memory.CompileResult {
	return memory.CompileResult{
		Op: memory.OpArchive,
		Decisions: []memory.Decision{
			{Field: memory.FieldArchiveScope, Disposition: memory.DispositionNative},
			{Field: memory.FieldArchiveOlderThan, Disposition: memory.DispositionNative},
			{Field: memory.FieldArchiveDestination, Disposition: memory.DispositionNative},
		},
	}
}
func (noopOps) ExecuteArchive(_ context.Context, _ memory.ArchiveRequest) (memory.ArchiveResponse, error) {
	return memory.ArchiveResponse{}, nil
}
