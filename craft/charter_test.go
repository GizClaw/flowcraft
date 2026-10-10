package craft

import (
	"testing"

	"github.com/GizClaw/flowcraft/craft/hostmcp"
	"github.com/GizClaw/flowcraft/craft/plugin"
)

// TestPermissionCharterCoverage scans the permission whitelist against
// both charter tables: every permission must be claimed either by a
// plugin contribution row or by a host primitive grant.
func TestPermissionCharterCoverage(t *testing.T) {
	t.Parallel()
	claimed := map[string]string{}
	for _, row := range plugin.Charter {
		claimed[row.Permission] = "plugin." + row.Kind
	}
	for _, row := range hostmcp.Charter {
		if row.Grant == "" {
			continue
		}
		claimed[row.Grant] = "hostmcp." + row.Tool
	}
	for _, permission := range plugin.ValidPermissions() {
		if _, ok := claimed[permission]; !ok {
			t.Fatalf("permission %q is not claimed by any charter row", permission)
		}
	}
}
