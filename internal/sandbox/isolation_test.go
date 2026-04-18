package sandbox

import (
	"runtime"
	"testing"
)

func skipIfNotLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("sandbox requires Linux (current: %s)", runtime.GOOS)
	}
}

func TestProbeIsolation_Deterministic(t *testing.T) {
	skipIfNotLinux(t)
	r1, err1 := probeIsolation()
	r2, err2 := probeIsolation()
	if (err1 == nil) != (err2 == nil) {
		t.Fatal("probeIsolation should return consistent error results")
	}
	if err1 == nil && r1.backend != r2.backend {
		t.Fatal("probeIsolation should return consistent results")
	}
}

func TestProbeIsolation_ErrorOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("only runs on non-Linux")
	}
	_, err := probeIsolation()
	if err == nil {
		t.Fatal("expected error on non-Linux")
	}
}

func TestProbeIsolation_SmokeTest(t *testing.T) {
	skipIfNotLinux(t)
	r, err := probeIsolation()
	if err != nil {
		t.Skipf("bwrap not available: %v", err)
	}
	t.Logf("probeIsolation: backend=%s, bwrapPath=%s", r.backend, r.bwrapPath)
	if r.bwrapPath == "" {
		t.Fatal("bubblewrap backend should have non-empty bwrapPath")
	}
}

func TestIsolationBackend_String(t *testing.T) {
	if got := backendBubblewrap.String(); got != "bubblewrap" {
		t.Fatalf("String() = %q, want %q", got, "bubblewrap")
	}
}
