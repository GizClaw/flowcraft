package journal

import (
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestSettingsBuildRejectsAVanishedExtraRoot: a writable path that is
// already unusable at build time is a configuration error, not a
// quietly smaller watch set.
func TestSettingsBuildRejectsAVanishedExtraRoot(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	_, err := (Settings{}).Build(root, []string{gone})
	if !errdefs.IsValidation(err) {
		t.Fatalf("Build = %v, want a validation error for the vanished writable path", err)
	}
}

// TestSettingsBuildRejectsAnAbsurdWatchSet mirrors the retention cap: a
// settings typo must fail the build rather than set the journal's
// resource ambition to the OS limit.
func TestSettingsBuildRejectsAnAbsurdWatchSet(t *testing.T) {
	root := t.TempDir()
	_, err := (Settings{MaxWatchSet: maxWatchSet + 1}).Build(root, nil)
	if !errdefs.IsValidation(err) {
		t.Fatalf("Build = %v, want a validation error for max_watch_set above the cap", err)
	}
}
