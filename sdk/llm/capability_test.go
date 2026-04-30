package llm

import "testing"

func TestModelCaps_Supports(t *testing.T) {
	caps := DisabledCaps(CapTemperature)
	if caps.Supports(CapTemperature) {
		t.Fatal("expected CapTemperature disabled")
	}
	if !caps.Supports(CapJSONSchema) {
		t.Fatal("expected CapJSONSchema supported")
	}
}

func TestModelCaps_IsZero(t *testing.T) {
	if !(ModelCaps{}).IsZero() {
		t.Fatal("zero-value should be IsZero")
	}
	if DisabledCaps(CapTemperature).IsZero() {
		t.Fatal("non-empty should not be IsZero")
	}
}
