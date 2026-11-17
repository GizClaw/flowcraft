package correlation

import "testing"

// TestAttrNamesAreStable pins the identity keys to their on-the-wire strings.
// Renaming any of these is a breaking change for every dashboard, alert and log
// query that filters on the key — bumping this test forces an explicit,
// reviewable acknowledgement, the same contract core/telemetry keeps for the keys
// that describe the work rather than its identity.
func TestAttrNamesAreStable(t *testing.T) {
	cases := []struct{ got, want string }{
		{AttrRuntimeID, "agent.runtime.id"},
		{AttrTenantID, "tenant.id"},
		{AttrSessionID, "session.id"},
		{AttrAgentName, "agent.name"},
		{AttrTurn, "turn"},
		{AttrManifestID, "agent.manifest.id"},
		{AttrTaskID, "task.id"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("attr constant changed: got %q, want %q", tc.got, tc.want)
		}
	}
}

// Two keys with different semantics must not resolve to the same string: a
// producer filtering on one would silently pick up the other.
func TestAttrNamesAreDistinct(t *testing.T) {
	names := []string{
		AttrRuntimeID, AttrTenantID, AttrSessionID,
		AttrAgentName, AttrTurn, AttrManifestID, AttrTaskID,
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, dup := seen[name]; dup {
			t.Fatalf("duplicate attribute key %q", name)
		}
		seen[name] = struct{}{}
	}
}
