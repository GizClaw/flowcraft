package telemetry

import "testing"

func TestTracerWithSuffix(t *testing.T) {
	tr := TracerWithSuffix("store")
	if tr == nil {
		t.Fatal("expected non-nil tracer")
	}
}

func TestTracerWithSuffix_Empty(t *testing.T) {
	tr := TracerWithSuffix("")
	if tr == nil {
		t.Fatal("expected non-nil tracer")
	}
}

func TestTracerWithSuffix_SlashTrimmed(t *testing.T) {
	tr := TracerWithSuffix("/store/")
	if tr == nil {
		t.Fatal("expected non-nil tracer")
	}
}

func TestTracerWithSuffix_Whitespace(t *testing.T) {
	tr := TracerWithSuffix("  ")
	if tr == nil {
		t.Fatal("expected non-nil tracer for whitespace-only suffix")
	}
}

func TestMeterWithSuffix(t *testing.T) {
	m := MeterWithSuffix("audio")
	if m == nil {
		t.Fatal("expected non-nil meter")
	}
}

func TestMeterWithSuffix_Empty(t *testing.T) {
	m := MeterWithSuffix("")
	if m == nil {
		t.Fatal("expected non-nil meter")
	}
}

func TestMeterWithSuffix_SlashTrimmed(t *testing.T) {
	m := MeterWithSuffix("/audio/")
	if m == nil {
		t.Fatal("expected non-nil meter")
	}
}

func TestLoggerDefault(t *testing.T) {
	l := Logger("")
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}
