package sandbox

import (
	"runtime"
	"testing"
)

func TestProbeIsolation_Deterministic(t *testing.T) {
	r1 := probeIsolation()
	r2 := probeIsolation()
	if r1.backend != r2.backend {
		t.Fatal("probeIsolation should return consistent results")
	}
}

func TestProbeIsolation_BareOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("only runs on non-Linux")
	}
	r := probeIsolation()
	if r.backend != backendBare {
		t.Fatalf("expected bare on %s, got %s", runtime.GOOS, r.backend)
	}
}

func TestProbeIsolation_SmokeTestCoversPermissions(t *testing.T) {
	r := probeIsolation()
	t.Logf("probeIsolation: backend=%s, bwrapPath=%s", r.backend, r.bwrapPath)

	if r.backend == backendBubblewrap && r.bwrapPath == "" {
		t.Fatal("bubblewrap backend should have non-empty bwrapPath")
	}
}

func TestIsolationBackend_String(t *testing.T) {
	tests := []struct {
		backend isolationBackend
		want    string
	}{
		{backendBare, "bare"},
		{backendBubblewrap, "bubblewrap"},
	}
	for _, tt := range tests {
		if got := tt.backend.String(); got != tt.want {
			t.Fatalf("String() = %q, want %q", got, tt.want)
		}
	}
}
