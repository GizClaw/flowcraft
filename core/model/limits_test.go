package model

import "testing"

func TestModelLimitsValidate(t *testing.T) {
	if err := (ModelLimits{}).Validate(); err != nil {
		t.Fatalf("empty limits: %v", err)
	}
	positive := 128_000
	if err := (ModelLimits{
		MaxInputTokens: &positive,
	}).Validate(); err != nil {
		t.Fatalf("positive limit: %v", err)
	}
	positiveOutput := 64_000
	if err := (ModelLimits{
		MaxOutputTokens: &positiveOutput,
	}).Validate(); err != nil {
		t.Fatalf("positive output limit: %v", err)
	}
	for _, value := range []int{0, -1} {
		limit := value
		if err := (ModelLimits{
			MaxInputTokens: &limit,
		}).Validate(); err == nil {
			t.Fatalf("limit %d unexpectedly accepted", value)
		}
		if err := (ModelLimits{
			MaxOutputTokens: &limit,
		}).Validate(); err == nil {
			t.Fatalf("output limit %d unexpectedly accepted", value)
		}
	}
}
