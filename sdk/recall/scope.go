package recall

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	retrievalns "github.com/GizClaw/flowcraft/sdk/retrieval/namespace"
)

var recallNamespace = retrievalns.MustRegister("ltm")

// NamespaceFor returns the retrieval namespace for a scope:
//
//	ltm_<runtime>__u<len>_<user>   when UserID != ""
//	ltm_<runtime>__global          otherwise
//
// AgentID is a soft-isolation dimension stored in metadata, not in the
// namespace, so a single agent can union its own facts with shared ones
// in a single recall call.
func NamespaceFor(s Scope) string {
	if s.UserID != "" {
		return recallNamespace.UserScope(s.RuntimeID, s.UserID)
	}
	return recallNamespace.GlobalScope(s.RuntimeID)
}

// saneNS is the pre-v0.5 local namespace sanitizer.
//
// Deprecated: use retrieval/namespace.Sanitize. This compatibility shim will
// be removed in v0.5.0 after recall namespace construction is fully centralised
// in sdk/retrieval/namespace.
func saneNS(s string) string {
	return retrievalns.Sanitize(s)
}

// AgentRecallFilter returns the default filter that limits hits to the agent
// that issued the recall PLUS any agent-shared entries (agent_id == "")
// .
//
// When AgentID is empty the filter is empty (cross-agent recall).
// namespacesForRecall returns the set of namespaces a single Recall
// call must visit, derived from scope.EffectivePartitions(). Order
// preserves the caller's partition list; duplicates are removed.
//
// Mapping (mirrors [NamespaceFor]):
//   - [PartitionUser]:   ltm_<rt>__u_<user>  (requires non-empty UserID; skipped otherwise)
//   - [PartitionGlobal]: ltm_<rt>__global
//
// When the resolved set is empty (e.g. PartitionUser on a scope
// with no UserID) the legacy single-namespace path is used as a
// fallback so callers asking for impossible buckets still get a
// well-formed query. This is the entry point that fixed #150 —
// pre-fix Recall ignored Partitions entirely and always issued
// against NamespaceFor(scope).
func namespacesForRecall(scope Scope) []string {
	parts := scope.EffectivePartitions()
	seen := make(map[string]struct{}, len(parts))
	var out []string
	for _, p := range parts {
		s := Scope{RuntimeID: scope.RuntimeID}
		switch p {
		case PartitionUser:
			if scope.UserID == "" {
				continue
			}
			s.UserID = scope.UserID
		case PartitionGlobal:
			// s.UserID stays empty → global namespace.
		default:
			continue
		}
		ns := NamespaceFor(s)
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	if len(out) == 0 {
		out = []string{NamespaceFor(scope)}
	}
	return out
}

func AgentRecallFilter(s Scope) retrieval.Filter {
	if s.AgentID == "" {
		return retrieval.Filter{}
	}
	return retrieval.Filter{Or: []retrieval.Filter{
		{Eq: map[string]any{"agent_id": s.AgentID}},
		{Eq: map[string]any{"agent_id": ""}},
	}}
}

// TombstoneFilter returns the default filter that excludes entries
// the LLM update resolver has marked for deletion via
// metadata[MetaTombstone] = true. The filter accepts entries where
// the field is missing or any value other than the boolean true.
//
// Recall composes this filter by default; callers that want to
// surface tombstoned entries (debugging the resolver, or staging an
// Auditable.Rollback) MUST set Request.WithTombstoned = true.
//
// MetaTombstone is a RESERVED metadata key owned by the recall
// package — pre-existing user data accidentally stored under
// "tombstone" will be hidden by Recall until the caller passes
// WithTombstoned.
func TombstoneFilter() retrieval.Filter {
	return retrieval.Filter{Or: []retrieval.Filter{
		{Missing: []string{MetaTombstone}},
		{Neq: map[string]any{MetaTombstone: true}},
	}}
}

// ExpireFilter returns the default filter that excludes expired entries
// . Pass current time via now (use time.Now() at call site).
func ExpireFilter(now time.Time) retrieval.Filter {
	return retrieval.Filter{Or: []retrieval.Filter{
		{Missing: []string{"expires_at"}},
		{Range: map[string]retrieval.Range{
			"expires_at": {Gt: now.UnixMilli()},
		}},
	}}
}

// MergeFilters returns a Filter that requires all non-empty inputs to match.
func MergeFilters(filters ...retrieval.Filter) retrieval.Filter {
	var out []retrieval.Filter
	for _, f := range filters {
		if !filterIsEmpty(f) {
			out = append(out, f)
		}
	}
	switch len(out) {
	case 0:
		return retrieval.Filter{}
	case 1:
		return out[0]
	default:
		return retrieval.Filter{And: out}
	}
}

func filterIsEmpty(f retrieval.Filter) bool {
	return len(f.And) == 0 && len(f.Or) == 0 && f.Not == nil &&
		len(f.Eq) == 0 && len(f.Neq) == 0 && len(f.In) == 0 && len(f.NotIn) == 0 &&
		len(f.Range) == 0 && len(f.Exists) == 0 && len(f.Missing) == 0 && len(f.Match) == 0 &&
		len(f.Contains) == 0 && len(f.IContains) == 0 && len(f.ContainsAny) == 0 && len(f.ContainsAll) == 0
}
