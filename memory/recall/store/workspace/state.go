package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
)

const stateVersion = 1

type state struct {
	Version      int                    `json:"version"`
	Facts        []domain.TemporalFact  `json:"facts,omitempty"`
	Evidence     []evidenceRecord       `json:"evidence,omitempty"`
	Observations []domain.Observation   `json:"observations,omitempty"`
	Links        []domain.FactLink      `json:"links,omitempty"`
	SideEffects  []sideEffectRecord     `json:"side_effects,omitempty"`
	Async        []asyncSemanticRecord  `json:"async_semantic,omitempty"`
	Counters     map[string]counterPair `json:"counters,omitempty"`
}

type counterPair struct {
	SideEffect    int `json:"side_effect,omitempty"`
	AsyncSemantic int `json:"async_semantic,omitempty"`
}

type evidenceRecord struct {
	Scope      domain.Scope       `json:"scope"`
	FactID     string             `json:"fact_id"`
	EvidenceID string             `json:"evidence_id"`
	Ordinal    int                `json:"ordinal"`
	Ref        domain.EvidenceRef `json:"ref"`
}

type sideEffectRecord struct {
	Job        port.SideEffectJob     `json:"job"`
	Status     string                 `json:"status"`
	EnqueuedAt time.Time              `json:"enqueued_at"`
	RetryAt    time.Time              `json:"retry_at,omitempty"`
	Failure    port.SideEffectFailure `json:"failure,omitempty"`
	Result     port.SideEffectResult  `json:"result,omitempty"`
}

type asyncSemanticRecord struct {
	Job        port.AsyncSemanticJob     `json:"job"`
	Status     string                    `json:"status"`
	EnqueuedAt time.Time                 `json:"enqueued_at"`
	Failure    port.AsyncSemanticFailure `json:"failure,omitempty"`
	Result     port.AsyncSemanticResult  `json:"result,omitempty"`
}

func newState() state {
	return state{Version: stateVersion, Counters: map[string]counterPair{}}
}

func encodeState(st state) ([]byte, error) {
	if st.Version == 0 {
		st.Version = stateVersion
	}
	if st.Counters == nil {
		st.Counters = map[string]counterPair{}
	}
	return json.MarshalIndent(st, "", "  ")
}

func decodeState(raw []byte) (state, error) {
	if len(raw) == 0 {
		return newState(), nil
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{}, err
	}
	if st.Version == 0 {
		st.Version = stateVersion
	}
	if st.Counters == nil {
		st.Counters = map[string]counterPair{}
	}
	return st, nil
}

func partitionCounter(st *state, scope domain.Scope) counterPair {
	if st.Counters == nil {
		st.Counters = map[string]counterPair{}
	}
	return st.Counters[scope.PartitionKey()]
}

func setPartitionCounter(st *state, scope domain.Scope, c counterPair) {
	if st.Counters == nil {
		st.Counters = map[string]counterPair{}
	}
	st.Counters[scope.PartitionKey()] = c
}

func incrementSideEffectCancelled(st *state, scope domain.Scope, n int) {
	if n <= 0 {
		return
	}
	c := partitionCounter(st, scope)
	c.SideEffect += n
	setPartitionCounter(st, scope, c)
}

func incrementAsyncCancelled(st *state, scope domain.Scope, n int) {
	if n <= 0 {
		return
	}
	c := partitionCounter(st, scope)
	c.AsyncSemantic += n
	setPartitionCounter(st, scope, c)
}

func newLeaseToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func sortFacts(facts []domain.TemporalFact) {
	sort.SliceStable(facts, func(i, j int) bool {
		if facts[i].ObservedAt.Equal(facts[j].ObservedAt) {
			return facts[i].ID < facts[j].ID
		}
		return facts[i].ObservedAt.Before(facts[j].ObservedAt)
	})
}

func cloneSideEffectJob(job port.SideEffectJob) port.SideEffectJob {
	return sqlstmt.CloneSideEffectJob(job)
}
