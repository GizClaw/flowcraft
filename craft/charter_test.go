package craft

import (
	"testing"

	"github.com/GizClaw/flowcraft/craft/plugin"
)

// TestPermissionCharterCoverage scans the permission whitelist against the
// contribution table: every permission craft accepts from a manifest has to
// be a row of that table, because a row is what pairs it with a gate and a
// drop rule. Grants for host primitives are not in the whitelist at all —
// they belong to the application's primitives (see [HostPrimitives]).
func TestPermissionCharterCoverage(t *testing.T) {
	t.Parallel()
	claimed := map[string]string{}
	for _, row := range plugin.Charter {
		claimed[row.Permission] = "plugin." + row.Kind
	}
	for _, permission := range plugin.ValidPermissions() {
		if _, ok := claimed[permission]; !ok {
			t.Fatalf("permission %q is not claimed by any charter row", permission)
		}
	}
}
