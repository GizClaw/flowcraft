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
	m.cleanupSaveGraphArtifacts(context.Background(), scope, state)
	return errdefs.Abortedf("recall: scope partition wiped during save")
}

func (m *memory) cleanupSaveGraphArtifacts(ctx context.Context, scope Scope, state *write.WriteState) {
	if m == nil || state == nil {
		return
	}
	if len(state.GraphLinkIDs) > 0 && m.linkStore != nil {
		_ = m.linkStore.Delete(ctx, scope, state.GraphLinkIDs)
	}
	observationIDs := uniqueNonEmptyStrings(state.RawObservationIDs, state.GraphObservationIDs)
	if len(observationIDs) == 0 {
		return
	}
	if m.observationProjection != nil {
		_ = m.observationProjection.ForgetObservations(ctx, scope, observationIDs)
	}
	if m.observationStore != nil {
		_ = m.observationStore.Delete(ctx, scope, observationIDs)
	}
}

func uniqueNonEmptyStrings(groups ...[]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, group := range groups {
		for _, value := range group {
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}
