package sqlstmt

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	StatusPending  = "pending"
	StatusLeased   = "leased"
	StatusFailed   = "failed"
	StatusComplete = "complete"

	CounterSideEffect    = "side_effect"
	CounterAsyncSemantic = "async_semantic"

	SideEffectLeaseTTL    = 30 * time.Second
	AsyncSemanticLeaseTTL = 5 * time.Minute
	AsyncRetryBackoff     = 30 * time.Second
)

var Schema = []string{
	`CREATE TABLE IF NOT EXISTS recall_facts (
		runtime_id text NOT NULL,
		user_id text NOT NULL,
		id text NOT NULL,
		kind text NOT NULL,
		observed_at_ns bigint NOT NULL,
		valid_to_ns bigint,
		closed integer NOT NULL DEFAULT 0,
		expires_at_ns bigint,
		merge_key text NOT NULL DEFAULT '',
		corrected_by text NOT NULL DEFAULT '',
		origin_request_id text NOT NULL DEFAULT '',
		payload_json text NOT NULL,
		PRIMARY KEY (runtime_id, user_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS recall_facts_list_idx ON recall_facts(runtime_id, user_id, observed_at_ns, id)`,
	`CREATE INDEX IF NOT EXISTS recall_facts_merge_idx ON recall_facts(runtime_id, user_id, merge_key, observed_at_ns, id)`,
	`CREATE INDEX IF NOT EXISTS recall_facts_corrected_idx ON recall_facts(runtime_id, user_id, corrected_by, observed_at_ns, id)`,
	`CREATE INDEX IF NOT EXISTS recall_facts_origin_idx ON recall_facts(runtime_id, user_id, origin_request_id, observed_at_ns, id)`,
	`CREATE TABLE IF NOT EXISTS recall_fact_entities (
		runtime_id text NOT NULL,
		user_id text NOT NULL,
		fact_id text NOT NULL,
		entity text NOT NULL,
		PRIMARY KEY (runtime_id, user_id, fact_id, entity)
	)`,
	`CREATE INDEX IF NOT EXISTS recall_fact_entities_scope_idx ON recall_fact_entities(runtime_id, user_id, entity, fact_id)`,
	`CREATE TABLE IF NOT EXISTS recall_evidence_refs (
		runtime_id text NOT NULL,
		user_id text NOT NULL,
		fact_id text NOT NULL,
		evidence_id text NOT NULL,
		ordinal integer NOT NULL,
		payload_json text NOT NULL,
		PRIMARY KEY (runtime_id, user_id, fact_id, evidence_id)
	)`,
	`CREATE INDEX IF NOT EXISTS recall_evidence_id_idx ON recall_evidence_refs(runtime_id, user_id, evidence_id)`,
	`CREATE INDEX IF NOT EXISTS recall_evidence_fact_idx ON recall_evidence_refs(runtime_id, user_id, fact_id, ordinal, evidence_id)`,
	`CREATE TABLE IF NOT EXISTS recall_observations (
		runtime_id text NOT NULL,
		user_id text NOT NULL,
		id text NOT NULL,
		kind text NOT NULL,
		source_id text NOT NULL DEFAULT '',
		observed_at_ns bigint NOT NULL,
		payload_json text NOT NULL,
		PRIMARY KEY (runtime_id, user_id, id)
	)`,
	`CREATE INDEX IF NOT EXISTS recall_observations_list_idx ON recall_observations(runtime_id, user_id, observed_at_ns, id)`,
	`CREATE INDEX IF NOT EXISTS recall_observations_kind_idx ON recall_observations(runtime_id, user_id, kind, observed_at_ns, id)`,
	`CREATE INDEX IF NOT EXISTS recall_observations_source_idx ON recall_observations(runtime_id, user_id, source_id, observed_at_ns, id)`,
	`CREATE TABLE IF NOT EXISTS recall_links (
		runtime_id text NOT NULL,
		user_id text NOT NULL,
		id text NOT NULL,
		type text NOT NULL,
		from_kind text NOT NULL,
		from_id text NOT NULL,
		to_kind text NOT NULL,
		to_id text NOT NULL,
		merge_key text NOT NULL DEFAULT '',
		created_at_ns bigint NOT NULL,
		payload_json text NOT NULL,
		PRIMARY KEY (runtime_id, user_id, id)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS recall_links_merge_idx ON recall_links(runtime_id, user_id, merge_key) WHERE merge_key <> ''`,
	`CREATE INDEX IF NOT EXISTS recall_links_from_idx ON recall_links(runtime_id, user_id, from_kind, from_id, created_at_ns, id)`,
	`CREATE INDEX IF NOT EXISTS recall_links_to_idx ON recall_links(runtime_id, user_id, to_kind, to_id, created_at_ns, id)`,
	`CREATE INDEX IF NOT EXISTS recall_links_type_idx ON recall_links(runtime_id, user_id, type, created_at_ns, id)`,
	`CREATE TABLE IF NOT EXISTS recall_queue_counters (
		kind text NOT NULL,
		runtime_id text NOT NULL,
		user_id text NOT NULL,
		cancelled_total integer NOT NULL DEFAULT 0,
		PRIMARY KEY (kind, runtime_id, user_id)
	)`,
	`CREATE TABLE IF NOT EXISTS recall_side_effect_jobs (
		id text PRIMARY KEY,
		request_id text NOT NULL,
		runtime_id text NOT NULL,
		user_id text NOT NULL,
		kind text NOT NULL,
		status text NOT NULL,
		enqueued_at_ns bigint NOT NULL,
		retry_at_ns bigint,
		lease_until_ns bigint,
		lease_token text NOT NULL DEFAULT '',
		attempt integer NOT NULL DEFAULT 0,
		failure_class text NOT NULL DEFAULT '',
		failure_err text NOT NULL DEFAULT '',
		result_json text NOT NULL DEFAULT '',
		payload_json text NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS recall_side_effect_claim_idx ON recall_side_effect_jobs(runtime_id, user_id, status, enqueued_at_ns, id)`,
	`CREATE INDEX IF NOT EXISTS recall_side_effect_request_idx ON recall_side_effect_jobs(request_id)`,
	`CREATE TABLE IF NOT EXISTS recall_async_semantic_jobs (
		request_id text PRIMARY KEY,
		runtime_id text NOT NULL,
		user_id text NOT NULL,
		status text NOT NULL,
		enqueued_at_ns bigint NOT NULL,
		lease_until_ns bigint,
		lease_token text NOT NULL DEFAULT '',
		attempt integer NOT NULL DEFAULT 0,
		failure_class text NOT NULL DEFAULT '',
		failure_err text NOT NULL DEFAULT '',
		result_json text NOT NULL DEFAULT '',
		payload_json text NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS recall_async_semantic_claim_idx ON recall_async_semantic_jobs(runtime_id, user_id, status, enqueued_at_ns, request_id)`,
	`CREATE TABLE IF NOT EXISTS recall_async_semantic_job_episodes (
		request_id text NOT NULL,
		runtime_id text NOT NULL,
		user_id text NOT NULL,
		episode_fact_id text NOT NULL,
		PRIMARY KEY (request_id, episode_fact_id)
	)`,
	`CREATE INDEX IF NOT EXISTS recall_async_semantic_episode_idx ON recall_async_semantic_job_episodes(runtime_id, user_id, episode_fact_id, request_id)`,
}

func Placeholders(start, n int, postgres bool) string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		if postgres {
			out[i] = fmt.Sprintf("$%d", start+i)
		} else {
			out[i] = "?"
		}
	}
	return strings.Join(out, ",")
}

func EncodeJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", errdefs.Validationf("recall sql store: encode payload: %v", err)
	}
	return string(b), nil
}

func DecodeJSON[T any](raw string) (T, error) {
	var out T
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return out, err
	}
	return out, nil
}

func ScopeParts(scope domain.Scope) (string, string) {
	return scope.RuntimeID, scope.UserID
}

func ScopeFromParts(runtimeID, userID string) domain.Scope {
	return domain.Scope{RuntimeID: runtimeID, UserID: userID}
}

func BoolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TimePtrNS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func UniqueNonEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func CloneSideEffectJob(job port.SideEffectJob) port.SideEffectJob {
	out := job
	if len(job.Facts) > 0 {
		out.Facts = make([]domain.TemporalFact, len(job.Facts))
		for i, f := range job.Facts {
			out.Facts[i] = f.Clone()
		}
	}
	return out
}

func SideEffectJobID(job port.SideEffectJob) string {
	if job.ID != "" {
		return job.ID
	}
	return fmt.Sprintf("%s|%s", job.RequestID, job.Kind)
}

func ScrubFacts(facts []domain.TemporalFact) []domain.TemporalFact {
	if len(facts) == 0 {
		return nil
	}
	out := make([]domain.TemporalFact, 0, len(facts))
	for _, f := range facts {
		out = append(out, domain.TemporalFact{
			ID:    f.ID,
			Scope: f.Scope,
			Kind:  f.Kind,
		})
	}
	return out
}

func ClampNonNeg(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}
