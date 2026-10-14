package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// DerivationState reports what a retention sweep has to know about one
// conversation's derivation: the work this processor still owes it.
//
// It answers "may a sweep run now", which is narrower than "has derivation
// caught up", and the difference is deliberate. Commits no pass has scanned yet
// do not unsettle it: a sweep only removes generations readers do not resolve,
// so pending commits are safe to sweep beside, and the pass that catches up
// converges the lanes with the generation it publishes. What is not safe is
// work this processor has started. A pass mid-flight converges and publishes a
// generation the sweep may just have emptied, and a generation whose facts the
// pass has written but not published is one the next pass publishes whatever
// the sweep did to it -- while derivation resumes after its own watermark, so
// the commits that generation already covered are never derived again.
type DerivationState struct {
	// InFlight is set while a pass over the conversation is running in this
	// processor. It clears when the pass returns, however it returns.
	InFlight bool `json:"in_flight"`
	// Unpublished is set while this processor has stored facts under its own
	// policy generation and no pass has published that generation: the run
	// stopped between writing the facts and the switch that serves them.
	Unpublished bool `json:"unpublished"`
}

// Settled reports whether a retention sweep can run over the conversation
// without racing the derivation this processor owes it. It is the precondition
// RequireSettled enforces and RetireGenerations refuses on.
func (state DerivationState) Settled() bool { return !state.InFlight && !state.Unpublished }

// ErrDerivationUnsettled is the sentinel a refused sweep unwraps: the sweep
// asked while derivation over the conversation was still moving. It is
// retryable, and it means the sweep retired nothing.
var ErrDerivationUnsettled = errors.New("memory worker: derivation is unsettled")

// UnsettledDerivationError reports which work was still moving when a sweep was
// refused, so a caller can tell a pass it can wait out from a generation its
// own derivation has to finish first.
type UnsettledDerivationError struct {
	ConversationID string
	State          DerivationState
}

// Error names the condition that refused the sweep.
func (err *UnsettledDerivationError) Error() string {
	if err == nil {
		return ErrDerivationUnsettled.Error()
	}
	reason := "the generation this processor derives under is not published"
	if err.State.InFlight {
		reason = "a derivation pass is running"
	}
	return fmt.Sprintf("memory worker: conversation %q: derivation is unsettled: %s", err.ConversationID, reason)
}

// Unwrap reports the sentinel, so callers match the refusal rather than its
// wording.
func (err *UnsettledDerivationError) Unwrap() error { return ErrDerivationUnsettled }

// DerivationState reports the derivation this processor owes one conversation.
// It does not mutate derivation state, and it reads only what the sweep itself
// would read.
func (processor *Processor) DerivationState(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
) (DerivationState, error) {
	if processor == nil {
		return DerivationState{}, errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return DerivationState{}, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return DerivationState{}, err
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return DerivationState{}, errors.New("memory worker: conversation id is required")
	}
	state := DerivationState{InFlight: processor.deriving(scope, conversationID)}
	generations, err := processor.facts.ListGenerations(ctx, scope, conversationID)
	if err != nil {
		return DerivationState{}, fmt.Errorf("memory worker: list derivation generations: %w", err)
	}
	if !slices.Contains(generations, processor.policyDigest) {
		// This pass has stored nothing yet, so there is no generation of its
		// own for a sweep to retire.
		return state, nil
	}
	active, found, err := processor.facts.ActiveGeneration(ctx, scope, conversationID)
	if err != nil {
		return DerivationState{}, fmt.Errorf("memory worker: read the active generation: %w", err)
	}
	state.Unpublished = !found || active != processor.policyDigest
	return state, nil
}

// RequireSettled refuses when derivation over one conversation has not settled,
// and returns nil when it has. It is the precondition of a retention sweep: a
// sweep lists the generations it retires before it retires them, and the pass
// that owns a generation writes and publishes it again whether or not the sweep
// emptied it -- while it resumes after its own watermark, so the commits that
// generation had already covered are not derived a second time.
//
// The caller's keep list is not consulted: it decides what survives a sweep,
// not whether one may run. A host that finds itself refused should derive again
// (ProcessConversation, ProcessScope, or Assembly.RunOnce) and sweep after the
// pass rather than beside it.
func (processor *Processor) RequireSettled(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
) error {
	state, err := processor.DerivationState(ctx, scope, conversationID)
	if err != nil {
		return err
	}
	if state.Settled() {
		return nil
	}
	return &UnsettledDerivationError{ConversationID: strings.TrimSpace(conversationID), State: state}
}

// ErrScopeUnderRepair is the sentinel a refused derivation pass unwraps: the
// scope's projection lanes are being rebuilt, and a pass that ran beside the
// rebuild would index entries into a lane the rebuild is about to replace. It
// is retryable, and it means the pass derived nothing.
var ErrScopeUnderRepair = errors.New("memory worker: the scope's projection lanes are under repair")

// ScopeUnderRepairError reports which scope refused a derivation pass.
type ScopeUnderRepairError struct{ Scope corememory.Scope }

// Error names the scope that is being rebuilt.
func (err *ScopeUnderRepairError) Error() string {
	if err == nil {
		return ErrScopeUnderRepair.Error()
	}
	return fmt.Sprintf("memory worker: scope %s/%s/%s: %s",
		err.Scope.RuntimeID, err.Scope.UserID, err.Scope.AgentID, ErrScopeUnderRepair)
}

// Unwrap reports the sentinel, so callers match the refusal rather than its
// wording.
func (err *ScopeUnderRepairError) Unwrap() error { return ErrScopeUnderRepair }

// BeginRepair marks one scope's projection lanes as being rebuilt and returns
// the release that clears the mark. It refuses -- with ErrDerivationUnsettled --
// while a pass over the scope is running, because that pass would index beside
// the rebuild; while the mark is held, ProcessScope and ProcessConversation
// refuse with ErrScopeUnderRepair rather than indexing against a lane being
// rewritten.
//
// Both markers are in-process, like the pass marker they pair with: the rebuild
// is the host's own call, and a durable lease would have to outlive a crashed
// run to be worth writing (a crashed rebuild would hold derivation off
// forever). A rebuild that holds the mark and fails still releases it, so the
// only cost of a failure is the work the rebuild did before it stopped.
func (processor *Processor) BeginRepair(ctx context.Context, scope corememory.Scope) (func(), error) {
	if processor == nil {
		return nil, errors.New("memory worker: processor is required")
	}
	if ctx == nil {
		return nil, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	processor.runningMu.Lock()
	defer processor.runningMu.Unlock()
	if conversationID, busy := processor.firstInFlightLocked(scope); busy {
		return nil, &UnsettledDerivationError{
			ConversationID: conversationID, State: DerivationState{InFlight: true},
		}
	}
	key := scopeKey(scope)
	if processor.repairing == nil {
		processor.repairing = make(map[string]int)
	}
	processor.repairing[key]++
	return func() {
		processor.runningMu.Lock()
		defer processor.runningMu.Unlock()
		processor.repairing[key]--
		if processor.repairing[key] <= 0 {
			delete(processor.repairing, key)
		}
	}, nil
}

// beginDerivation marks a pass over one conversation as running and returns the
// release that clears it, or refuses when the scope's lanes are under repair.
// Both markers are taken under one lock, so a pass and a rebuild cannot both
// decide the scope is free.
func (processor *Processor) beginDerivation(scope corememory.Scope, conversationID string) (func(), error) {
	processor.runningMu.Lock()
	defer processor.runningMu.Unlock()
	if processor.repairing[scopeKey(scope)] > 0 {
		return nil, &ScopeUnderRepairError{Scope: scope}
	}
	key := derivationKey(scope, conversationID)
	if processor.running == nil {
		processor.running = make(map[string]int)
	}
	processor.running[key]++
	return func() {
		processor.runningMu.Lock()
		defer processor.runningMu.Unlock()
		processor.running[key]--
		if processor.running[key] <= 0 {
			delete(processor.running, key)
		}
	}, nil
}

// firstInFlightLocked reports one pass running anywhere in the scope: a
// conversation pass, or the document stream pass, which marks itself under the
// stream's own id. The caller holds the marker lock.
func (processor *Processor) firstInFlightLocked(scope corememory.Scope) (string, bool) {
	prefix := scopeKey(scope) + "\x00"
	for key := range processor.running {
		if strings.HasPrefix(key, prefix) {
			return strings.TrimPrefix(key, prefix), true
		}
	}
	return "", false
}

// underRepair reports whether a rebuild of the scope's projection lanes is in
// flight in this processor.
func (processor *Processor) underRepair(scope corememory.Scope) bool {
	processor.runningMu.Lock()
	defer processor.runningMu.Unlock()
	return processor.repairing[scopeKey(scope)] > 0
}

// scopeKey identifies one hard scope inside the marker maps.
func scopeKey(scope corememory.Scope) string {
	return strings.Join([]string{scope.RuntimeID, scope.UserID, scope.AgentID}, "\x00")
}

// deriving reports whether a pass over one conversation is running.
func (processor *Processor) deriving(scope corememory.Scope, conversationID string) bool {
	processor.runningMu.Lock()
	defer processor.runningMu.Unlock()
	return processor.running[derivationKey(scope, conversationID)] > 0
}

// derivationKey identifies one conversation inside one hard scope: a processor
// serves every registered scope, so a conversation id alone is not unique.
func derivationKey(scope corememory.Scope, conversationID string) string {
	return strings.Join([]string{scope.RuntimeID, scope.UserID, scope.AgentID, conversationID}, "\x00")
}
