package memory

import "context"

// NoopRuntime is a Runtime whose every operation is satisfied by
// a no-op implementation. It returns zero values, does not
// persist anything, and is safe for concurrent use.
//
// Use it when an agent is deployed without a memory backend, in
// tests that need a Runtime value but no real storage, and as the
// default fallback when a deploy document references a memory
// hook without binding a memory.Assembly resource.
type NoopRuntime struct {
	*Runtime
}

// NewNoopRuntime constructs a NoopRuntime backed by no-op
// implementations of all six operations.
func NewNoopRuntime(spec Spec) (*NoopRuntime, error) {
	noop := noopOps{}
	rt, err := New(spec, Impls{
		Append:  noop,
		Load:    noop,
		Recall:  noop,
		Import:  noop,
		Compact: noop,
		Archive: noop,
	})
	if err != nil {
		return nil, err
	}
	return &NoopRuntime{Runtime: rt}, nil
}

// noopOps is a single value that satisfies all six op interfaces.
// Its Compile methods emit one Native Decision per canonical field
// the runtime considers active, so the runtime's ledger check
// always passes. Execute returns the zero response.
type noopOps struct{}

func (noopOps) CompileAppend(_ context.Context, req AppendRequest) CompileResult {
	fields := appendActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, f := range fields {
		decisions[i] = nativeDecision(f)
	}
	return CompileResult{Op: OpAppend, Decisions: decisions}
}

func (noopOps) ExecuteAppend(_ context.Context, _ AppendRequest) (AppendResponse, error) {
	return AppendResponse{}, nil
}

func (noopOps) CompileLoad(_ context.Context, req LoadRequest) CompileResult {
	fields := loadActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, f := range fields {
		decisions[i] = nativeDecision(f)
	}
	return CompileResult{Op: OpLoad, Decisions: decisions}
}

func (noopOps) ExecuteLoad(_ context.Context, _ LoadRequest) (LoadResponse, error) {
	return LoadResponse{}, nil
}

func (noopOps) CompileRecall(_ context.Context, req RecallRequest) CompileResult {
	fields := recallActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, f := range fields {
		decisions[i] = nativeDecision(f)
	}
	return CompileResult{Op: OpRecall, Decisions: decisions}
}

func (noopOps) ExecuteRecall(_ context.Context, _ RecallRequest) (RecallResponse, error) {
	return RecallResponse{}, nil
}

func (noopOps) CompileImport(_ context.Context, req ImportRequest) CompileResult {
	fields := importActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, f := range fields {
		decisions[i] = nativeDecision(f)
	}
	return CompileResult{Op: OpImport, Decisions: decisions}
}

func (noopOps) ExecuteImport(_ context.Context, _ ImportRequest) (ImportResponse, error) {
	return ImportResponse{}, nil
}

func (noopOps) CompileCompact(_ context.Context, req CompactRequest) CompileResult {
	fields := compactActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, f := range fields {
		decisions[i] = nativeDecision(f)
	}
	return CompileResult{Op: OpCompact, Decisions: decisions}
}

func (noopOps) ExecuteCompact(_ context.Context, _ CompactRequest) (CompactResponse, error) {
	return CompactResponse{}, nil
}

func (noopOps) CompileArchive(_ context.Context, req ArchiveRequest) CompileResult {
	fields := archiveActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, f := range fields {
		decisions[i] = nativeDecision(f)
	}
	return CompileResult{Op: OpArchive, Decisions: decisions}
}

func (noopOps) ExecuteArchive(_ context.Context, _ ArchiveRequest) (ArchiveResponse, error) {
	return ArchiveResponse{}, nil
}
