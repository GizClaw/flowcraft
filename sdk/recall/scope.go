package recall

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// NamespaceFor returns the retrieval namespace for a scope:
//
//	ltm_<runtime>__u_<user>   when UserID != ""
//	ltm_<runtime>__global     otherwise
//
// AgentID is a soft-isolation dimension stored in metadata, not in the
// namespace, so a single agent can union its own facts with shared ones
// in a single recall call.
func NamespaceFor(s Scope) string {
	rt := s.RuntimeID
	if rt == "" {
		rt = "anon"
	}
	if s.UserID != "" {
		return "ltm_" + saneNS(rt) + "__u_" + saneNS(s.UserID)
	}
	return "ltm_" + saneNS(rt) + "__global"
}

// saneNS replaces non [A-Za-z0-9_] chars with '_' so namespace satisfies
// adapter validation (sqlite/postgres §6.2/§6.3).
func saneNS(s string) string {
	if s == "" {
		return "anon"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "anon"
	}
	return b.String()
}

// AgentRecallFilter returns the default filter that limits hits to the agent
// that issued the recall PLUS any agent-shared entries (agent_id == "")
// .
//
// When AgentID is empty the filter is empty (cross-agent recall).
func AgentRecallFilter(s Scope) retrieval.Filter {
	if s.AgentID == "" {
		return retrieval.Filter{}
	}
	return retrieval.Filter{Or: []retrieval.Filter{
		{Eq: map[string]any{"agent_id": s.AgentID}},
		{Eq: map[string]any{"agent_id": ""}},
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
