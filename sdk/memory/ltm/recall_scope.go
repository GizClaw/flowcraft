package ltm

// MemoryPartition names a logical partition for long-term List/Search (recall path).
// It is orthogonal to [MemoryScope] (row persistence). Implementations combine
// partitions as a union: an entry matches if it satisfies any listed partition.
type MemoryPartition string

const (
	// PartitionUser matches rows whose Scope.UserID equals RecallScope.UserID
	// (and SessionID rules when RecallScope.SessionID is non-empty).
	PartitionUser MemoryPartition = "user"
	// PartitionGlobal matches runtime-global rows (empty UserID and SessionID on the entry).
	PartitionGlobal MemoryPartition = "global"
)

// RecallScope describes which partitions to include in a List or Search call.
// At least one partition should be set; [NormalizePartitions] defaults to [PartitionUser].
type RecallScope struct {
	RuntimeID  string            `json:"runtime_id,omitempty"`
	UserID     string            `json:"user_id,omitempty"`
	SessionID  string            `json:"session_id,omitempty"`
	Partitions []MemoryPartition `json:"partitions,omitempty"`
}

// NormalizePartitions returns a deduplicated slice; empty input defaults to user-only.
func NormalizePartitions(parts []MemoryPartition) []MemoryPartition {
	if len(parts) == 0 {
		return []MemoryPartition{PartitionUser}
	}
	seen := make(map[MemoryPartition]struct{}, len(parts))
	out := make([]MemoryPartition, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return []MemoryPartition{PartitionUser}
	}
	return out
}

// normalizeRecall fills RuntimeID and partitions for use after options coalescing.
func normalizeRecall(r *RecallScope, runtimeID string) *RecallScope {
	if r == nil {
		return nil
	}
	out := *r
	if out.RuntimeID == "" {
		out.RuntimeID = runtimeID
	}
	out.Partitions = NormalizePartitions(out.Partitions)
	return &out
}

// EffectiveRecallForList builds the active recall scope for List from [ListOptions].
// If opts.Recall is set, it wins (normalized). Else if opts.Scope is set: global scope
// maps to PartitionGlobal only; otherwise user partition with that scope's UserID/SessionID.
// If both are nil, returns nil (legacy: no scope filter).
func EffectiveRecallForList(opts ListOptions, runtimeID string) *RecallScope {
	if opts.Recall != nil {
		return normalizeRecall(opts.Recall, runtimeID)
	}
	if opts.Scope != nil {
		if opts.Scope.IsGlobal() {
			return normalizeRecall(&RecallScope{
				RuntimeID:  runtimeID,
				Partitions: []MemoryPartition{PartitionGlobal},
			}, runtimeID)
		}
		return normalizeRecall(&RecallScope{
			RuntimeID:  runtimeID,
			UserID:     opts.Scope.UserID,
			SessionID:  opts.Scope.SessionID,
			Partitions: []MemoryPartition{PartitionUser},
		}, runtimeID)
	}
	return nil
}

// EffectiveRecallForSearch is the Search counterpart of [EffectiveRecallForList].
func EffectiveRecallForSearch(opts SearchOptions, runtimeID string) *RecallScope {
	if opts.Recall != nil {
		return normalizeRecall(opts.Recall, runtimeID)
	}
	if opts.Scope != nil {
		if opts.Scope.IsGlobal() {
			return normalizeRecall(&RecallScope{
				RuntimeID:  runtimeID,
				Partitions: []MemoryPartition{PartitionGlobal},
			}, runtimeID)
		}
		return normalizeRecall(&RecallScope{
			RuntimeID:  runtimeID,
			UserID:     opts.Scope.UserID,
			SessionID:  opts.Scope.SessionID,
			Partitions: []MemoryPartition{PartitionUser},
		}, runtimeID)
	}
	return nil
}

// CacheKey returns a stable key for assembler recall caches (partitions order-independent).
func (r *RecallScope) CacheKey() string {
	if r == nil {
		return ""
	}
	parts := NormalizePartitions(r.Partitions)
	pk := ""
	for i, p := range parts {
		if i > 0 {
			pk += ","
		}
		pk += string(p)
	}
	if r.SessionID != "" {
		return r.RuntimeID + "|rec|" + pk + "|" + r.UserID + "|" + r.SessionID
	}
	if r.UserID != "" {
		return r.RuntimeID + "|rec|" + pk + "|" + r.UserID
	}
	return r.RuntimeID + "|rec|" + pk
}

// EntryMatchesRecallScope reports whether e belongs to any partition in r.
// nil r matches all entries (legacy).
func EntryMatchesRecallScope(e *MemoryEntry, r *RecallScope) bool {
	if r == nil {
		return true
	}
	for _, p := range NormalizePartitions(r.Partitions) {
		switch p {
		case PartitionGlobal:
			if e.Scope.IsGlobal() {
				return true
			}
		case PartitionUser:
			if matchUserPartition(e, r.UserID, r.SessionID) {
				return true
			}
		}
	}
	return false
}

func matchUserPartition(e *MemoryEntry, userID, querySessionID string) bool {
	if userID == "" {
		return false
	}
	if e.Scope.UserID != userID {
		return false
	}
	if querySessionID != "" {
		return e.Scope.SessionID == querySessionID || e.Scope.SessionID == ""
	}
	return true
}
