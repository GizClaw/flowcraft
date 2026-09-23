package seatbelt

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox"
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
			name: "exclude outside the root",
			body: `{"exclude": ["../elsewhere"]}`,
			want: errdefs.IsValidation,
		},
		{
			name: "valid settings",
			body: `{"exclude": ["dist"], "ops": ["create", "remove"]}`,
			// Accepted on a platform with a watch source; the
			// sandbox-exec binary itself may still be missing, which is
			// a different (also reported) reason.
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

// TestFactoryJournalSettingsBuildARunnerWithAJournal is the positive
// half: on a host that can run the backend at all, the journal settings
// produce a runner whose capabilities report the attached journal.
func TestFactoryJournalSettingsBuildARunnerWithAJournal(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skipf("sandbox-exec is not installed: %v", err)
	}
	root := t.TempDir()
	quoted, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := NewFactory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{"root": ` + string(quoted) + `, "journal": {"retention": 32}}`),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	runner, ok := value.(sandbox.Runner)
	if !ok {
		t.Fatalf("factory returned %T, want a sandbox.Runner", value)
	}
	t.Cleanup(func() { _ = runner.Close() })
	if !runner.Capabilities().Journal.Enabled {
		t.Fatal("journal settings produced a runner without a journal")
	}
	reader, err := sandbox.OpenJournal(context.Background(), runner)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
