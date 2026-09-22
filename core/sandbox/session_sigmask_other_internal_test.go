//go:build unix && !linux && !darwin

package sandbox

import (
	"errors"
	"testing"
)

// TestStartWithCleanSignalMaskDegradesWithoutAMaskAPI pins the documented
// degradation: a platform with no mask API still spawns, and the spawn's
// own error comes back unchanged.
func TestStartWithCleanSignalMaskDegradesWithoutAMaskAPI(t *testing.T) {
	if _, err := replaceThreadSignalMask(emptySigset()); err == nil {
		t.Fatal("replaceThreadSignalMask succeeded on a platform with no mask API")
	}

	sentinel := errors.New("spawn refused")
	if err := startWithCleanSignalMask(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("startWithCleanSignalMask = %v, want the spawn's own error", err)
	}

	ran := false
	if err := startWithCleanSignalMask(func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("startWithCleanSignalMask = %v, want nil", err)
	}
	if !ran {
		t.Fatal("startWithCleanSignalMask skipped the spawn")
	}
}
