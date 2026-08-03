package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Clock returns the current time. Tests inject a frozen clock
// through Spec; production code uses the system clock.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock is the default Clock implementation. Spec.Clock
// defaults to it when nil.
var SystemClock Clock = systemClock{}

// Spec captures the runtime-level configuration that is not
// per-op. The deployment layer (sdkx/memory) builds it from
// memory.yaml; the kernel does not accept free-form options
// beyond what is declared here.
type Spec struct {
	// RuntimeID is the hard partition name. The runtime
	// rejects constructions whose Spec.RuntimeID does not
	// match the Scope's RuntimeID on every operation.
	RuntimeID string
	// DefaultScope is the Scope the runtime applies when a
	// hook does not provide one explicitly. RuntimeID must
	// agree with Spec.RuntimeID.
	DefaultScope Scope
	// Clock is injectable for tests. Defaults to SystemClock.
	Clock Clock
	// DefaultLoadLimit caps Load requests that arrive without
	// an explicit Limit. A Load with Limit == 0 falls back to
	// this value, then to FallbackLoadLimit if it is also 0.
	DefaultLoadLimit int
	// DefaultTopK caps Recall requests that arrive without an
	// explicit TopK. Same fallback rule.
	DefaultTopK int
	// FallbackLoadLimit is the hard ceiling used when both
	// DefaultLoadLimit and the request Limit are 0. Set to a
	// non-zero value in production to prevent unbounded
	// loads.
	FallbackLoadLimit int
	// FallbackTopK is the hard ceiling for TopK.
	FallbackTopK int
}

// Validate enforces the Spec invariants. RuntimeID must be
// non-empty; the DefaultScope's RuntimeID must agree if set.
func (s Spec) Validate() error {
	if s.RuntimeID == "" {
		return errdefs.Validationf("memory: Spec.RuntimeID is required")
	}
	if strings.ContainsRune(s.RuntimeID, '\x00') {
		return errdefs.Validationf("memory: Spec.RuntimeID must not contain NUL")
	}
	if !s.DefaultScope.IsZero() {
		if err := s.DefaultScope.Validate(); err != nil {
			return fmt.Errorf("memory: Spec.DefaultScope: %w", err)
		}
	}
	if s.DefaultScope.RuntimeID != "" && s.DefaultScope.RuntimeID != s.RuntimeID {
		return errdefs.Validationf(
			"memory: Spec.DefaultScope.RuntimeID %q does not match Spec.RuntimeID %q",
			s.DefaultScope.RuntimeID, s.RuntimeID,
		)
	}
	return nil
}

// Impls groups the per-op implementations a runtime is built
// with. Each field is optional; calling an op whose Impl is nil
// returns KindNotConfigured.
//
// Hooks typically obtain the runtime from a deploy Source, and
// that source supplies an Impls built from the deploy document's
// resources. The kernel never accepts a partially configured
// runtime silently — every op Compile result reflects whether its
// impl is wired in.
type Impls struct {
	Append  AppendOp
	Load    LoadOp
	Recall  RecallOp
	Import  ImportOp
	Compact CompactOp
	Archive ArchiveOp
}

// CloseFunc is called by Runtime.Close to release resources held
// by an impl. It is invoked in the reverse order the operations
// were registered, and errors from each are joined.
type CloseFunc func() error

// Runtime is the instance-owned entry point for memory operations.
// It is built once per deploy and shared by every agent Instance
// and Run that depends on it. Close releases it; it is
// idempotent.
//
// A Runtime has no goroutines of its own; it is safe for
// concurrent use by multiple goroutines provided the underlying
// Impls are.
type Runtime struct {
	spec  Spec
	impls Impls
	mu    sync.Mutex
	// orderedClose records the Close funcs in the order they
	// were registered, so Close walks them in reverse.
	orderedClose []CloseFunc
	closed       bool
	inFlight     sync.WaitGroup
	closeDone    chan struct{}
	closeErr     error
}

// New constructs a Runtime. Spec and Impls are copied by value;
// later mutations of the caller's struct do not affect the
// runtime. The returned Runtime is ready for use.
//
// registerClose lets the caller (typically the deploy layer)
// attach cleanup funcs that Release should invoke in reverse
// order. It is safe to call before the first Execute.
func New(spec Spec, impls Impls) (*Runtime, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Clock == nil {
		spec.Clock = SystemClock
	}
	return &Runtime{spec: spec, impls: impls, closeDone: make(chan struct{})}, nil
}

// Spec returns the runtime's Spec. Diagnostics and configuration
// inspectors read it; mutating the returned value is a no-op for
// the runtime.
func (r *Runtime) Spec() Spec { return r.spec }

// RegisterClose records a cleanup func. Close invokes registered
// funcs in reverse registration order. Safe to call from the
// deploy layer after New and before the runtime is shared.
func (r *Runtime) RegisterClose(close CloseFunc) {
	if close == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = close()
		return
	}
	r.orderedClose = append(r.orderedClose, close)
	r.mu.Unlock()
}

// Close releases the runtime. It is idempotent; concurrent calls
// after the first return nil. Errors from individual Close funcs
// are joined via errdefs.Join.
func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		done := r.closeDone
		r.mu.Unlock()
		<-done
		r.mu.Lock()
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.closed = true
	pending := r.orderedClose
	r.orderedClose = nil
	r.mu.Unlock()

	r.inFlight.Wait()
	var errs []error
	for _, p := range slices.Backward(pending) {
		if err := p(); err != nil {
			errs = append(errs, err)
		}
	}
	var closeErr error
	if len(errs) > 0 {
		closeErr = errors.Join(errs...)
	}
	r.mu.Lock()
	r.closeErr = closeErr
	close(r.closeDone)
	r.mu.Unlock()
	return closeErr
}

func (r *Runtime) beginExecute(op Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return newError(KindNotConfigured, op, "", errors.New("memory: runtime is closed"))
	}
	r.inFlight.Add(1)
	return nil
}

func (r *Runtime) endExecute() {
	r.inFlight.Done()
}

// --- Compile / Execute wiring -----------------------------------
//
// Every op follows the same pattern:
//   1. The runtime validates the request (Scope, op-specific
//      shape) and decides whether each canonical field is active.
//   2. The runtime calls the registered Impl's Compile method.
//   3. The runtime enforces the ledger: any active field with no
//      decision, or any Rejected decision, blocks Execute.
//   4. The runtime calls Execute and returns the result.
//
// The kernel keeps the active-field set small and explicit so
// adding a new op is a matter of declaring its FieldIDs and
// implementing the activeFields helper.

func (r *Runtime) requireScope(op Operation, s Scope) error {
	if err := s.Validate(); err != nil {
		return newError(KindScopeInvalid, op, "", err)
	}
	if s.RuntimeID != r.spec.RuntimeID {
		return newError(
			KindScopeInvalid, op, "",
			fmt.Errorf("memory: Scope.RuntimeID %q does not match runtime %q",
				s.RuntimeID, r.spec.RuntimeID),
		)
	}
	return nil
}

func (r *Runtime) applyScopeDefault(s Scope) Scope {
	if s.IsZero() {
		return r.spec.DefaultScope
	}
	return s
}

func (r *Runtime) scopeDecision(op Operation, field FieldID, s Scope) Decision {
	if err := r.requireScope(op, s); err != nil {
		return rejectedDecision(field, ReasonScopeInvalid, "Scope is invalid for this runtime")
	}
	return nativeDecision(field)
}

func notConfiguredResult(op Operation, active []FieldID) CompileResult {
	decisions := make([]Decision, len(active))
	for i, field := range active {
		decisions[i] = rejectedDecision(
			field,
			ReasonNotConfigured,
			fmt.Sprintf("memory: %s op has no registered impl", op),
		)
	}
	return CompileResult{Op: op, Decisions: decisions}
}

// combineCompile overlays runtime policy decisions on the implementation
// ledger. Policy rejection wins; otherwise the implementation's decision is
// authoritative. A malformed implementation ledger is returned unchanged so
// enforceLedger can classify the programming error as KindInternal.
func (r *Runtime) combineCompile(
	op Operation,
	active []FieldID,
	policy CompileResult,
	impl CompileResult,
) CompileResult {
	if err := r.validateLedgerShape(op, policy, active); err != nil {
		return policy
	}
	if err := r.validateLedgerShape(op, impl, active); err != nil {
		return impl.Clone()
	}
	policyByField := make(map[FieldID]Decision, len(active))
	implByField := make(map[FieldID]Decision, len(active))
	for _, decision := range policy.Decisions {
		policyByField[decision.Field] = decision
	}
	for _, decision := range impl.Decisions {
		implByField[decision.Field] = decision
	}
	out := CompileResult{Op: op, Decisions: make([]Decision, 0, len(active))}
	for _, field := range active {
		if decision := policyByField[field]; decision.Disposition == DispositionRejected {
			out.Decisions = append(out.Decisions, decision)
		} else {
			out.Decisions = append(out.Decisions, implByField[field])
		}
	}
	return out
}

func (r *Runtime) validateLedgerShape(op Operation, result CompileResult, active []FieldID) error {
	if result.Op != op {
		return fmt.Errorf("memory: %s compile reported wrong op %q", op, result.Op)
	}
	seen := make(map[FieldID]struct{}, len(result.Decisions))
	activeSet := make(map[FieldID]struct{}, len(active))
	for _, field := range active {
		activeSet[field] = struct{}{}
	}
	for _, decision := range result.Decisions {
		if _, ok := activeSet[decision.Field]; !ok {
			return fmt.Errorf("memory: %s compile covered inactive field %q", op, decision.Field)
		}
		if _, duplicate := seen[decision.Field]; duplicate {
			return fmt.Errorf("memory: %s compile has duplicate decision for %q", op, decision.Field)
		}
		seen[decision.Field] = struct{}{}
	}
	for _, field := range active {
		if _, ok := seen[field]; !ok {
			return fmt.Errorf("memory: %s compile has no decision for active field %q", op, field)
		}
	}
	return nil
}

// enforceLedger verifies that every active field has a Native
// decision and that no Rejected decision slipped through. A
// missing or duplicate decision is an internal error (the
// implementation forgot to walk its ledger).
func (r *Runtime) enforceLedger(op Operation, result CompileResult, active []FieldID) error {
	if err := r.validateLedgerShape(op, result, active); err != nil {
		return newError(KindInternal, op, "", err)
	}
	for _, d := range result.Decisions {
		switch d.Disposition {
		case DispositionNative:
			if d.Reason != "" {
				return newError(
					KindInternal, op, d.Field,
					fmt.Errorf("memory: %s native decision must not carry reason %q", op, d.Reason),
				)
			}
		case DispositionRejected:
			if d.Reason == "" {
				return newError(
					KindInternal, op, d.Field,
					fmt.Errorf("memory: %s rejected decision must carry a reason", op),
				)
			}
		default:
			return newError(
				KindInternal, op, d.Field,
				fmt.Errorf("memory: %s compile returned invalid disposition %q", op, d.Disposition),
			)
		}
	}
	if rej, ok := result.Rejected(); ok {
		return newError(
			rejectionKind(rej.Reason), op, rej.Field,
			fmt.Errorf("memory: %s compile rejected: %s", op, rej.Message),
		)
	}
	return nil
}

func interruptedError(ctx context.Context, op Operation, err error) error {
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return newError(KindOperationInterrupted, op, "", err)
	}
	if ctx != nil && ctx.Err() != nil {
		return newError(KindOperationInterrupted, op, "", ctx.Err())
	}
	return nil
}

func (r *Runtime) requireRunnableContext(ctx context.Context, op Operation) error {
	return interruptedError(ctx, op, nil)
}

func wrapExecuteErr(ctx context.Context, op Operation, err error) error {
	if err == nil {
		return nil
	}
	if interrupted := interruptedError(ctx, op, err); interrupted != nil {
		return interrupted
	}
	return wrapErr(KindProviderFailure, op, "", err)
}

// rejectionKind maps a Compile Rejected.Reason to the
// corresponding ErrorKind. Unknown reasons fall back to
// KindUnsupportedFeature, which is the right default for
// "the impl says no".
func rejectionKind(r Reason) ErrorKind {
	switch r {
	case ReasonInvalidExtension:
		return KindInvalidExtension
	case ReasonInvalidValue:
		return KindInvalidRequest
	case ReasonNotConfigured:
		return KindNotConfigured
	case ReasonPolicyDenied:
		return KindPolicyDenied
	case ReasonScopeInvalid:
		return KindScopeInvalid
	default:
		return KindUnsupportedFeature
	}
}

// --- Append -----------------------------------------------------

// appendActiveFields returns the canonical fields the Append op
// considers "active" in this request. Every canonical field is
// always active: a required field like Records must be in the
// ledger even when empty so the compile can Reject the missing
// value. Optional fields are also in the ledger so callers can
// inspect the disposition of every field.
func appendActiveFields(_ AppendRequest) []FieldID {
	return []FieldID{
		FieldAppendScope,
		FieldAppendConversationID,
		FieldAppendIdempotencyKey,
		FieldAppendRecords,
		FieldAppendMetadata,
	}
}

func (r *Runtime) compileAppend(ctx context.Context, req AppendRequest) (AppendRequest, CompileResult) {
	req.Scope = r.applyScopeDefault(req.Scope)
	active := appendActiveFields(req)
	result := CompileResult{Op: OpAppend, Decisions: make([]Decision, 0, 5)}
	result.Decisions = append(result.Decisions,
		r.scopeDecision(OpAppend, FieldAppendScope, req.Scope),
		nativeDecision(FieldAppendConversationID),
		nativeDecision(FieldAppendIdempotencyKey),
	)
	if len(req.Records) > 0 {
		if err := validateAppendRecordIDs(req.Records); err != nil {
			result.Decisions = append(result.Decisions, rejectedDecision(
				FieldAppendRecords, ReasonInvalidValue, err.Error(),
			))
		} else {
			result.Decisions = append(result.Decisions, nativeDecision(FieldAppendRecords))
		}
	} else {
		result.Decisions = append(result.Decisions, rejectedDecision(
			FieldAppendRecords, ReasonInvalidValue, "Append.Records is required",
		))
	}
	result.Decisions = append(result.Decisions, nativeDecision(FieldAppendMetadata))
	if r.impls.Append == nil {
		return req, notConfiguredResult(OpAppend, active)
	}
	return req, r.combineCompile(
		OpAppend, active, result, r.impls.Append.CompileAppend(ctx, req),
	)
}

// CompileAppend returns the authoritative compile ledger after runtime
// defaults and policy have been applied and the configured implementation has
// compiled the effective request.
func (r *Runtime) CompileAppend(ctx context.Context, req AppendRequest) CompileResult {
	if err := r.beginExecute(OpAppend); err != nil {
		return notConfiguredResult(OpAppend, appendActiveFields(req))
	}
	defer r.endExecute()
	_, result := r.compileAppend(ctx, req)
	return result
}

// ExecuteAppend runs the Append op end-to-end. The runtime
// first runs its own policy compile (which validates shape and
// applies defaults), then the impl's compile (which decides
// wire-level support), then enforces both ledgers before
// calling the impl. A nil impl returns KindNotConfigured; a
// missing decision in either ledger returns KindInternal.
func (r *Runtime) ExecuteAppend(ctx context.Context, req AppendRequest) (AppendResponse, error) {
	if err := r.beginExecute(OpAppend); err != nil {
		return AppendResponse{}, err
	}
	defer r.endExecute()
	if err := r.requireRunnableContext(ctx, OpAppend); err != nil {
		return AppendResponse{}, err
	}
	req, compiled := r.compileAppend(ctx, req)
	if err := r.enforceLedger(OpAppend, compiled, appendActiveFields(req)); err != nil {
		return AppendResponse{}, err
	}
	if err := r.requireRunnableContext(ctx, OpAppend); err != nil {
		return AppendResponse{}, err
	}
	records, err := prepareAppendRecords(req.Records)
	if err != nil {
		kind := KindInternal
		if errors.Is(err, errDuplicateRecordID) {
			kind = KindInvalidRequest
		}
		return AppendResponse{}, newError(kind, OpAppend, FieldAppendRecords, err)
	}
	req.Records = records
	resp, err := r.impls.Append.ExecuteAppend(ctx, req)
	if err != nil {
		return AppendResponse{}, wrapExecuteErr(ctx, OpAppend, err)
	}
	return resp, nil
}

// --- Load --------------------------------------------------------

func loadActiveFields(_ LoadRequest) []FieldID {
	return []FieldID{
		FieldLoadScope,
		FieldLoadConversationID,
		FieldLoadCursor,
		FieldLoadLimit,
		FieldLoadReverse,
	}
}

// CompileLoad produces the compile decision ledger for a Load
// request. The runtime fills in the Limit field automatically
// when the request is unbounded: the request Limit (if non-zero),
// else Spec.DefaultLoadLimit, else Spec.FallbackLoadLimit.
func (r *Runtime) compileLoad(ctx context.Context, req LoadRequest) (LoadRequest, CompileResult) {
	req.Scope = r.applyScopeDefault(req.Scope)
	if req.Limit == 0 {
		if r.spec.DefaultLoadLimit > 0 {
			req.Limit = r.spec.DefaultLoadLimit
		} else if r.spec.FallbackLoadLimit > 0 {
			req.Limit = r.spec.FallbackLoadLimit
		}
	}
	active := loadActiveFields(req)
	result := CompileResult{Op: OpLoad, Decisions: make([]Decision, 0, 5)}
	result.Decisions = append(result.Decisions,
		r.scopeDecision(OpLoad, FieldLoadScope, req.Scope),
		nativeDecision(FieldLoadConversationID),
		nativeDecision(FieldLoadCursor),
		nativeDecision(FieldLoadReverse),
	)
	if req.Limit > 0 {
		result.Decisions = append(result.Decisions, nativeDecision(FieldLoadLimit))
	} else {
		result.Decisions = append(result.Decisions, rejectedDecision(
			FieldLoadLimit, ReasonInvalidValue,
			"Load.Limit is required (set on the request, Spec.DefaultLoadLimit, or Spec.FallbackLoadLimit)",
		))
	}
	if r.impls.Load == nil {
		return req, notConfiguredResult(OpLoad, active)
	}
	return req, r.combineCompile(
		OpLoad, active, result, r.impls.Load.CompileLoad(ctx, req),
	)
}

// CompileLoad compiles the effective request, including the materialized
// default Limit passed to the implementation.
func (r *Runtime) CompileLoad(ctx context.Context, req LoadRequest) CompileResult {
	if err := r.beginExecute(OpLoad); err != nil {
		return notConfiguredResult(OpLoad, loadActiveFields(req))
	}
	defer r.endExecute()
	_, result := r.compileLoad(ctx, req)
	return result
}

// ExecuteLoad runs the Load op. The runtime materialises the
// effective Limit on the request before the policy compile so
// the active-field set reflects what the impl will actually see.
func (r *Runtime) ExecuteLoad(ctx context.Context, req LoadRequest) (LoadResponse, error) {
	if err := r.beginExecute(OpLoad); err != nil {
		return LoadResponse{}, err
	}
	defer r.endExecute()
	if err := r.requireRunnableContext(ctx, OpLoad); err != nil {
		return LoadResponse{}, err
	}
	req, compiled := r.compileLoad(ctx, req)
	if err := r.enforceLedger(OpLoad, compiled, loadActiveFields(req)); err != nil {
		return LoadResponse{}, err
	}
	if err := r.requireRunnableContext(ctx, OpLoad); err != nil {
		return LoadResponse{}, err
	}
	resp, err := r.impls.Load.ExecuteLoad(ctx, req)
	if err != nil {
		return LoadResponse{}, wrapExecuteErr(ctx, OpLoad, err)
	}
	return resp, nil
}

// --- Recall ------------------------------------------------------

func recallActiveFields(_ RecallRequest) []FieldID {
	return []FieldID{
		FieldRecallScope,
		FieldRecallConversationID,
		FieldRecallQuery,
		FieldRecallTopK,
		FieldRecallFilters,
		FieldRecallMinScore,
	}
}

func (r *Runtime) compileRecall(ctx context.Context, req RecallRequest) (RecallRequest, CompileResult) {
	req.Scope = r.applyScopeDefault(req.Scope)
	if req.TopK == 0 {
		if r.spec.DefaultTopK > 0 {
			req.TopK = r.spec.DefaultTopK
		} else if r.spec.FallbackTopK > 0 {
			req.TopK = r.spec.FallbackTopK
		}
	}
	active := recallActiveFields(req)
	result := CompileResult{Op: OpRecall, Decisions: make([]Decision, 0, 6)}
	result.Decisions = append(result.Decisions,
		r.scopeDecision(OpRecall, FieldRecallScope, req.Scope),
		nativeDecision(FieldRecallConversationID),
		nativeDecision(FieldRecallFilters),
		nativeDecision(FieldRecallMinScore),
	)
	if req.Query != "" {
		result.Decisions = append(result.Decisions, nativeDecision(FieldRecallQuery))
	} else {
		result.Decisions = append(result.Decisions, rejectedDecision(
			FieldRecallQuery, ReasonInvalidValue, "Recall.Query is required",
		))
	}
	if req.TopK > 0 {
		result.Decisions = append(result.Decisions, nativeDecision(FieldRecallTopK))
	} else {
		result.Decisions = append(result.Decisions, rejectedDecision(
			FieldRecallTopK, ReasonInvalidValue,
			"Recall.TopK is required (set on the request, Spec.DefaultTopK, or Spec.FallbackTopK)",
		))
	}
	if r.impls.Recall == nil {
		return req, notConfiguredResult(OpRecall, active)
	}
	return req, r.combineCompile(
		OpRecall, active, result, r.impls.Recall.CompileRecall(ctx, req),
	)
}

// CompileRecall compiles the effective request, including the materialized
// default TopK passed to the implementation.
func (r *Runtime) CompileRecall(ctx context.Context, req RecallRequest) CompileResult {
	if err := r.beginExecute(OpRecall); err != nil {
		return notConfiguredResult(OpRecall, recallActiveFields(req))
	}
	defer r.endExecute()
	_, result := r.compileRecall(ctx, req)
	return result
}

func (r *Runtime) ExecuteRecall(ctx context.Context, req RecallRequest) (RecallResponse, error) {
	if err := r.beginExecute(OpRecall); err != nil {
		return RecallResponse{}, err
	}
	defer r.endExecute()
	if err := r.requireRunnableContext(ctx, OpRecall); err != nil {
		return RecallResponse{}, err
	}
	req, compiled := r.compileRecall(ctx, req)
	if err := r.enforceLedger(OpRecall, compiled, recallActiveFields(req)); err != nil {
		return RecallResponse{}, err
	}
	if err := r.requireRunnableContext(ctx, OpRecall); err != nil {
		return RecallResponse{}, err
	}
	resp, err := r.impls.Recall.ExecuteRecall(ctx, req)
	if err != nil {
		return RecallResponse{}, wrapExecuteErr(ctx, OpRecall, err)
	}
	return resp, nil
}

// --- Import ------------------------------------------------------

func importActiveFields(_ ImportRequest) []FieldID {
	return []FieldID{
		FieldImportScope,
		FieldImportDatasetID,
		FieldImportSource,
		FieldImportTags,
		FieldImportChunkPolicy,
	}
}

func (r *Runtime) compileImport(ctx context.Context, req ImportRequest) (ImportRequest, CompileResult) {
	req.Scope = r.applyScopeDefault(req.Scope)
	active := importActiveFields(req)
	result := CompileResult{Op: OpImport, Decisions: make([]Decision, 0, 5)}
	result.Decisions = append(result.Decisions,
		r.scopeDecision(OpImport, FieldImportScope, req.Scope),
		nativeDecision(FieldImportDatasetID),
		nativeDecision(FieldImportTags),
		nativeDecision(FieldImportChunkPolicy),
	)
	if req.Source != "" {
		result.Decisions = append(result.Decisions, nativeDecision(FieldImportSource))
	} else {
		result.Decisions = append(result.Decisions, rejectedDecision(
			FieldImportSource, ReasonInvalidValue, "Import.Source is required",
		))
	}
	if r.impls.Import == nil {
		return req, notConfiguredResult(OpImport, active)
	}
	return req, r.combineCompile(
		OpImport, active, result, r.impls.Import.CompileImport(ctx, req),
	)
}

// CompileImport compiles the effective request against runtime policy and the
// configured implementation.
func (r *Runtime) CompileImport(ctx context.Context, req ImportRequest) CompileResult {
	if err := r.beginExecute(OpImport); err != nil {
		return notConfiguredResult(OpImport, importActiveFields(req))
	}
	defer r.endExecute()
	_, result := r.compileImport(ctx, req)
	return result
}

func (r *Runtime) ExecuteImport(ctx context.Context, req ImportRequest) (ImportResponse, error) {
	if err := r.beginExecute(OpImport); err != nil {
		return ImportResponse{}, err
	}
	defer r.endExecute()
	if err := r.requireRunnableContext(ctx, OpImport); err != nil {
		return ImportResponse{}, err
	}
	req, compiled := r.compileImport(ctx, req)
	if err := r.enforceLedger(OpImport, compiled, importActiveFields(req)); err != nil {
		return ImportResponse{}, err
	}
	if err := r.requireRunnableContext(ctx, OpImport); err != nil {
		return ImportResponse{}, err
	}
	resp, err := r.impls.Import.ExecuteImport(ctx, req)
	if err != nil {
		return ImportResponse{}, wrapExecuteErr(ctx, OpImport, err)
	}
	return resp, nil
}

// --- Compact -----------------------------------------------------

func compactActiveFields(req CompactRequest) []FieldID {
	return []FieldID{FieldCompactScope, FieldCompactOlderThan, FieldCompactKeep}
}

func (r *Runtime) compileCompact(ctx context.Context, req CompactRequest) (CompactRequest, CompileResult) {
	req.Scope = r.applyScopeDefault(req.Scope)
	active := compactActiveFields(req)
	result := CompileResult{Op: OpCompact, Decisions: make([]Decision, 0, 3)}
	result.Decisions = append(result.Decisions,
		r.scopeDecision(OpCompact, FieldCompactScope, req.Scope))
	if req.OlderThan.IsZero() {
		result.Decisions = append(result.Decisions, rejectedDecision(
			FieldCompactOlderThan, ReasonInvalidValue, "Compact.OlderThan is required"))
	} else {
		result.Decisions = append(result.Decisions, nativeDecision(FieldCompactOlderThan))
	}
	if req.Keep < 0 {
		result.Decisions = append(result.Decisions, rejectedDecision(
			FieldCompactKeep, ReasonInvalidValue, "Compact.Keep must not be negative"))
	} else {
		result.Decisions = append(result.Decisions, nativeDecision(FieldCompactKeep))
	}
	if r.impls.Compact == nil {
		return req, notConfiguredResult(OpCompact, active)
	}
	return req, r.combineCompile(
		OpCompact, active, result, r.impls.Compact.CompileCompact(ctx, req),
	)
}

// CompileCompact compiles the effective request against runtime policy and
// the configured implementation.
func (r *Runtime) CompileCompact(ctx context.Context, req CompactRequest) CompileResult {
	if err := r.beginExecute(OpCompact); err != nil {
		return notConfiguredResult(OpCompact, compactActiveFields(req))
	}
	defer r.endExecute()
	_, result := r.compileCompact(ctx, req)
	return result
}

func (r *Runtime) ExecuteCompact(ctx context.Context, req CompactRequest) (CompactResponse, error) {
	if err := r.beginExecute(OpCompact); err != nil {
		return CompactResponse{}, err
	}
	defer r.endExecute()
	if err := r.requireRunnableContext(ctx, OpCompact); err != nil {
		return CompactResponse{}, err
	}
	req, compiled := r.compileCompact(ctx, req)
	if err := r.enforceLedger(OpCompact, compiled, compactActiveFields(req)); err != nil {
		return CompactResponse{}, err
	}
	if err := r.requireRunnableContext(ctx, OpCompact); err != nil {
		return CompactResponse{}, err
	}
	resp, err := r.impls.Compact.ExecuteCompact(ctx, req)
	if err != nil {
		return CompactResponse{}, wrapExecuteErr(ctx, OpCompact, err)
	}
	return resp, nil
}

// --- Archive -----------------------------------------------------

func archiveActiveFields(_ ArchiveRequest) []FieldID {
	return []FieldID{FieldArchiveScope, FieldArchiveOlderThan, FieldArchiveDestination}
}

func (r *Runtime) compileArchive(ctx context.Context, req ArchiveRequest) (ArchiveRequest, CompileResult) {
	req.Scope = r.applyScopeDefault(req.Scope)
	active := archiveActiveFields(req)
	result := CompileResult{Op: OpArchive, Decisions: make([]Decision, 0, 3)}
	result.Decisions = append(result.Decisions,
		r.scopeDecision(OpArchive, FieldArchiveScope, req.Scope))
	if req.OlderThan.IsZero() {
		result.Decisions = append(result.Decisions, rejectedDecision(
			FieldArchiveOlderThan, ReasonInvalidValue, "Archive.OlderThan is required"))
	} else {
		result.Decisions = append(result.Decisions, nativeDecision(FieldArchiveOlderThan))
	}
	if strings.TrimSpace(req.Destination) == "" {
		result.Decisions = append(result.Decisions, rejectedDecision(
			FieldArchiveDestination, ReasonInvalidValue, "Archive.Destination is required"))
	} else {
		result.Decisions = append(result.Decisions, nativeDecision(FieldArchiveDestination))
	}
	if r.impls.Archive == nil {
		return req, notConfiguredResult(OpArchive, active)
	}
	return req, r.combineCompile(
		OpArchive, active, result, r.impls.Archive.CompileArchive(ctx, req),
	)
}

// CompileArchive compiles the effective request against runtime policy and
// the configured implementation.
func (r *Runtime) CompileArchive(ctx context.Context, req ArchiveRequest) CompileResult {
	if err := r.beginExecute(OpArchive); err != nil {
		return notConfiguredResult(OpArchive, archiveActiveFields(req))
	}
	defer r.endExecute()
	_, result := r.compileArchive(ctx, req)
	return result
}

func (r *Runtime) ExecuteArchive(ctx context.Context, req ArchiveRequest) (ArchiveResponse, error) {
	if err := r.beginExecute(OpArchive); err != nil {
		return ArchiveResponse{}, err
	}
	defer r.endExecute()
	if err := r.requireRunnableContext(ctx, OpArchive); err != nil {
		return ArchiveResponse{}, err
	}
	req, compiled := r.compileArchive(ctx, req)
	if err := r.enforceLedger(OpArchive, compiled, archiveActiveFields(req)); err != nil {
		return ArchiveResponse{}, err
	}
	if err := r.requireRunnableContext(ctx, OpArchive); err != nil {
		return ArchiveResponse{}, err
	}
	resp, err := r.impls.Archive.ExecuteArchive(ctx, req)
	if err != nil {
		return ArchiveResponse{}, wrapExecuteErr(ctx, OpArchive, err)
	}
	return resp, nil
}

// --- shared helpers ---------------------------------------------

var errDuplicateRecordID = errors.New("memory: duplicate Record.ID")

func validateAppendRecordIDs(records []Record) error {
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		if record.ID == "" {
			continue
		}
		if _, duplicate := seen[record.ID]; duplicate {
			return fmt.Errorf("%w %q", errDuplicateRecordID, record.ID)
		}
		seen[record.ID] = struct{}{}
	}
	return nil
}

func prepareAppendRecords(records []Record) ([]Record, error) {
	if err := validateAppendRecordIDs(records); err != nil {
		return nil, err
	}
	out := append([]Record(nil), records...)
	seen := make(map[string]struct{}, len(out))
	for _, record := range out {
		if record.ID != "" {
			seen[record.ID] = struct{}{}
		}
	}
	for i := range out {
		if out[i].ID != "" {
			continue
		}
		for {
			var raw [16]byte
			if _, err := rand.Read(raw[:]); err != nil {
				return nil, fmt.Errorf("memory: generate Record.ID: %w", err)
			}
			id := hex.EncodeToString(raw[:])
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			out[i].ID = id
			seen[id] = struct{}{}
			break
		}
	}
	return out, nil
}

// wrapErr attaches a memory ErrorKind to an error returned from
// an Impl. The original cause is preserved via Unwrap so callers
// can errors.As it.
func wrapErr(kind ErrorKind, op Operation, field FieldID, err error) error {
	if err == nil {
		return nil
	}
	if AsError(err) != nil {
		// Already a memory error; do not double-wrap.
		return err
	}
	return newError(kind, op, field, err)
}
