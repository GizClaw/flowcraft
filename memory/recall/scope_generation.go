package recall

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// peekScopeGen returns the current wipe generation for the scope's
// canonical (runtime, user) partition.
func (m *memory) peekScopeGen(scope Scope) uint64 {
	key := writeScopeKey{runtimeID: scope.RuntimeID, userID: scope.UserID}
	m.scopeGenMu.Lock()
	defer m.scopeGenMu.Unlock()
	if m.scopeGen == nil {
		return 0
	}
	return m.scopeGen[key]
}

// bumpScopeGen advances the partition generation. ForgetAll(Hard) and
// ExpireRetired call this before deleting canonical rows so in-flight
// Saves that started before the wipe cannot append afterward.
func (m *memory) bumpScopeGen(scope Scope) {
	key := writeScopeKey{runtimeID: scope.RuntimeID, userID: scope.UserID}
	m.scopeGenMu.Lock()
	defer m.scopeGenMu.Unlock()
	if m.scopeGen == nil {
		m.scopeGen = make(map[writeScopeKey]uint64)
	}
	m.scopeGen[key]++
}

// enterScopeWrite acquires the scope write lock and refuses entry when
// the partition generation changed since startGen (another caller
// completed ForgetAll / ExpireRetired while this Save ran ingest).
func (m *memory) enterScopeWrite(scope Scope, startGen uint64) (func(), error) {
	unlock := m.lockWriteScope(scope)
	if m.peekScopeGen(scope) != startGen {
		unlock()
		return nil, errdefs.Abortedf("recall: scope partition generation changed before write lock")
	}
	return unlock, nil
}

func (m *memory) holdWriteTelemetry() {
	if m != nil && m.deferredTelemetry != nil {
		m.deferredTelemetry.Hold()
	}
}

func (m *memory) flushWriteTelemetry() {
	if m != nil && m.deferredTelemetry != nil {
		m.deferredTelemetry.Flush()
	}
}

// abortIfScopeGenChanged rolls back appended facts when the partition
// was wiped mid-post-runner.
func (m *memory) abortIfScopeGenChanged(scope Scope, lockedGen uint64, state *write.WriteState) error {
	if m.peekScopeGen(scope) == lockedGen {
		return nil
	}
	if state != nil && len(state.AppendedFactIDs) > 0 {
		_ = m.store.Delete(context.Background(), scope, state.AppendedFactIDs)
	}
	return errdefs.Abortedf("recall: scope partition wiped during save")
}
