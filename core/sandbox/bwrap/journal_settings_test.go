package bwrap

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
)

// TestFactoryJournalSettingsTakeEffect is the positive half of the
// deployment path: when the factory does return a runner (bwrap
// present), the journal really is attached and the numbers from the
// document are the ones in force. Without it, dropping the wiring from
// the factory — or the decode of the journal subtree — would leave
// every lane green.
func TestFactoryJournalSettingsTakeEffect(t *testing.T) {
	if !journal.Available() {
		t.Skipf("no file-watch source on this platform")
	}
	root := t.TempDir()
	quoted, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := NewFactory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{"root": ` + string(quoted) + `, "journal": {"max_watch_set": 512, "exclude": ["dist"]}}`),
	})
	if err != nil {
		if errdefs.IsNotAvailable(err) {
			t.Skipf("bwrap is not usable here: %v", err)
		}
		t.Fatalf("build: %v", err)
	}
	runner, ok := value.(sandbox.Runner)
	if !ok {
		t.Fatalf("factory returned %T, want a sandbox.Runner", value)
	}
	t.Cleanup(func() { _ = runner.Close() })

	caps := runner.Capabilities().Journal
	if !caps.Enabled {
		t.Fatal("journal settings produced a runner without a journal")
	}
	if caps.WatchBudget != 512 {
		t.Fatalf("WatchBudget = %d, want the configured 512", caps.WatchBudget)
	}
	reader, err := sandbox.OpenJournal(context.Background(), runner)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

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
