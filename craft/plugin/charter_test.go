package plugin

import (
	"reflect"
	"strings"
	"testing"
)

// TestCharterMatchesManifest scans the contribution table against the
// Manifest struct and the permission whitelist in both directions.
func TestCharterMatchesManifest(t *testing.T) {
	t.Parallel()
	manifestType := reflect.TypeOf(Manifest{})
	claimed := map[string]string{}
	kinds := map[string]bool{}
	for _, row := range Charter {
		if strings.TrimSpace(row.Kind) == "" || kinds[row.Kind] {
			t.Fatalf("charter kind %q is empty or duplicated", row.Kind)
		}
		kinds[row.Kind] = true
		if _, ok := validPermissions[row.Permission]; !ok {
			t.Fatalf("charter %s: unknown permission %q", row.Kind, row.Permission)
		}
		if _, duplicate := claimed[row.Permission]; duplicate {
			t.Fatalf("permission %q claimed twice", row.Permission)
		}
		// Every row records what a missing grant does, exempted rows
		// included: "nothing is dropped here" is itself a decision.
		if strings.TrimSpace(row.DropRule) == "" {
			t.Fatalf("charter %s: no drop rule recorded for a missing grant",
				row.Kind)
		}
		if row.Exemption != "" && strings.TrimSpace(row.Exemption) == "" {
			t.Fatalf("charter %s: exemption is blank", row.Kind)
		}
		claimed[row.Permission] = row.Kind
		if row.ManifestField != "" {
			if _, ok := manifestType.FieldByName(row.ManifestField); !ok {
				t.Fatalf("charter %s: Manifest has no field %q",
					row.Kind, row.ManifestField)
			}
		}
	}
	if len(ValidPermissions()) != len(validPermissions) {
		t.Fatal("ValidPermissions does not cover the whitelist")
	}
}
