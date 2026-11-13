package runtime

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox/journal"
)

// A backend's own settings block is only meaningful with that backend: a block
// that does nothing is a deployment running somewhere its author did not describe.
func TestSandboxBackendBlocksAreBackendSpecific(t *testing.T) {
	cases := map[string]struct {
		apply func(*SandboxConfig)
		want  string
	}{
		"bwrap block on local": {
			apply: func(c *SandboxConfig) { c.Bwrap = &BwrapConfig{Binary: "/usr/bin/bwrap"} },
			want:  "sandbox.bwrap",
		},
		"seatbelt block on local": {
			apply: func(c *SandboxConfig) { c.Seatbelt = &SeatbeltConfig{ReadOnlyRoot: true} },
			want:  "sandbox.seatbelt",
		},
		"windows block on local": {
			apply: func(c *SandboxConfig) { c.Windows = &WindowsConfig{WriteConfine: true} },
			want:  "sandbox.windows",
		},
		"bwrap block on remote": {
			apply: func(c *SandboxConfig) {
				c.Backend = BackendRemote
				c.Remote = RemoteConfig{Endpoint: "https://sandbox.test"}
				c.Bwrap = &BwrapConfig{ExtraFlags: []string{"--die-with-parent"}}
			},
			want: "sandbox.bwrap",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t.TempDir())
			tc.apply(&cfg.Sandbox)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("a backend block was accepted on the wrong backend")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// The journal block's cheap invariants fail at load time, where the field is still
// visible; root resolution and the platform's watch source stay the build's job.
func TestSandboxJournalBlockValidates(t *testing.T) {
	cases := map[string]struct {
		journal *journal.Settings
		want    string
	}{
		"unknown op": {
			journal: &journal.Settings{Ops: []string{"chmod"}},
			want:    "sandbox.journal.ops",
		},
		"negative retention": {
			journal: &journal.Settings{Retention: -1},
			want:    "sandbox.journal.retention",
		},
		"negative watch budget": {
			journal: &journal.Settings{MaxWatchSet: -1},
			want:    "sandbox.journal.max_watch_set",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t.TempDir())
			cfg.Sandbox.Journal = tc.journal
			err := cfg.Validate()
			if err == nil {
				t.Fatal("a malformed journal block was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}

	// The zero block is valid: presence is the opt-in, defaults are the settings.
	cfg := validConfig(t.TempDir())
	cfg.Sandbox.Journal = &journal.Settings{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an empty journal block was rejected: %v", err)
	}
}

// A configured journal is attached to the runner and reachable through the
// runtime, which is what makes the block do something instead of merely parse.
func TestSandboxJournalBlockAttachesAJournal(t *testing.T) {
	cfg := localConfig(t)
	cfg.Sandbox.Journal = &journal.Settings{Exclude: []string{".git", "node_modules"}}

	rt, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if rt.Journal() == nil {
		t.Fatal("the configured journal was not attached")
	}

	// Absence stays the default: no block, no journal.
	plain, err := New(t.Context(), localConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = plain.Close() }()
	if plain.Journal() != nil {
		t.Fatal("a runtime without a journal block attached one")
	}
}
