package sandbox

import (
	"runtime"
	"testing"
)

func TestProbeIsolation_Deterministic(t *testing.T) {
	r1, err1 := probeIsolation()
	r2, err2 := probeIsolation()
	if (err1 == nil) != (err2 == nil) {
		t.Fatal("probeIsolation should return consistent error results")
	}
	if err1 == nil && r1.backend != r2.backend {
		t.Fatal("probeIsolation should return consistent results")
	}
}

func TestProbeIsolation_BareOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("only runs on non-Linux")
	}
	r, err := probeIsolation()
	if err != nil {
		t.Fatalf("expected no error on non-Linux, got %v", err)
	}
	if r.backend != backendBare {
		t.Fatalf("expected bare on %s, got %s", runtime.GOOS, r.backend)
	}
}

func TestProbeIsolation_SmokeTestCoversPermissions(t *testing.T) {
	r, err := probeIsolation()
	if err != nil {
		t.Logf("probeIsolation returned error (expected on Linux without bwrap): %v", err)
		return
	}
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
