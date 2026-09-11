package model

import (
	"strings"
	"testing"
	"time"
)

func TestModelLifecycleValidateFor(t *testing.T) {
	retiresAt := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	model := ModelID{Provider: "p", Name: "m"}
	replacement := ModelID{Provider: "p", Name: "m2"}
	cases := []struct {
		name      string
		lifecycle ModelLifecycle
		want      string
	}{
		{
			name:      "zero value is active",
			lifecycle: ModelLifecycle{},
		},
		{
			name:      "active cannot carry retirement metadata",
			lifecycle: ModelLifecycle{RetiresAt: &retiresAt},
			want:      "active model cannot carry retirement metadata",
		},
		{
			name: "deprecated carries its record",
			lifecycle: ModelLifecycle{
				Status:      ModelStatusDeprecated,
				RetiresAt:   &retiresAt,
				Replacement: &replacement,
			},
		},
		{
			name:      "unknown status",
			lifecycle: ModelLifecycle{Status: ModelStatus("gone")},
			want:      `unknown model status "gone"`,
		},
		{
			name: "replacement must differ from the model",
			lifecycle: ModelLifecycle{
				Status:      ModelStatusDeprecated,
				Replacement: &model,
			},
			want: "replacement must differ from the model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.lifecycle.ValidateFor(model)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateFor() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateFor() = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestModelLifecycleCloneDoesNotAlias pins that the clone owns its pointers.
func TestModelLifecycleCloneDoesNotAlias(t *testing.T) {
	retiresAt := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	original := ModelLifecycle{Status: ModelStatusRetired, RetiresAt: &retiresAt}
	clone := original.Clone()
	if clone.RetiresAt == original.RetiresAt {
		t.Fatal("clone shares the RetiresAt pointer")
	}
	*clone.RetiresAt = clone.RetiresAt.Add(time.Hour)
	if original.RetiresAt.Equal(*clone.RetiresAt) {
		t.Fatal("mutating the clone changed the original")
	}
}
