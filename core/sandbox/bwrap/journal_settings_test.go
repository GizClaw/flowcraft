package bwrap

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
)

// TestFactoryJournalSettings checks the deployment path: the journal
// subtree is decoded strictly and resolved during the build, so a
// misconfiguration fails there instead of producing a runner that
// quietly watches nothing.
func TestFactoryJournalSettings(t *testing.T) {
	root := t.TempDir()
	settings := func(body string) resource.Input {
		t.Helper()
		quoted, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		return resource.Input{
			Settings: json.RawMessage(`{"root": ` + string(quoted) + `, "journal": ` + body + `}`),
		}
	}

	cases := []struct {
		name string
		body string
		want func(error) bool
	}{
		{
			name: "unknown op",
			body: `{"ops": ["chmod"]}`,
			want: errdefs.IsValidation,
		},
		{
			name: "unknown field",
			body: `{"retention": 8, "nope": 1}`,
			want: errdefs.IsValidation,
		},
		{
			name: "negative watch budget",
			body: `{"max_watch_set": -3}`,
			want: errdefs.IsValidation,
		},
		{
			name: "valid settings",
			body: `{"exclude": ["dist"], "ops": ["create", "remove"]}`,
			// Accepted on a platform with a watch source; the bwrap
			// binary itself may still be missing, which is a different
			// (also reported) reason.
			want: func(err error) bool {
				if journal.Available() {
					return err == nil || errdefs.IsNotAvailable(err)
				}
				return errdefs.IsNotAvailable(err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewFactory().New(context.Background(), settings(tc.body))
			if tc.want(err) {
				return
			}
			t.Fatalf("build returned %v", err)
		})
	}
}
