package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference/usage"
)

func TestAddUsageSumsTokensCacheAndCost(t *testing.T) {
	cache := int64(2)
	cost := usage.Money{Currency: "USD", Units: 10, Scale: 4}
	total := usage.Usage{}
	addUsage(&total, usage.Usage{
		InputTokens: 5, OutputTokens: 7, TotalTokens: 12,
		Input: usage.InputTokenUsage{CacheReadTokens: &cache},
	})
	addUsage(&total, usage.Usage{
		InputTokens: 1, OutputTokens: 2, TotalTokens: 3,
		Billing: &usage.BillingUsage{Cost: &cost},
	})
	if total.InputTokens != 6 || total.OutputTokens != 9 || total.TotalTokens != 15 {
		t.Errorf("tokens = %+v, want 6/9/15", total)
	}
	if total.Input.CacheReadTokens == nil || *total.Input.CacheReadTokens != 2 {
		t.Errorf("cache read = %v, want 2", total.Input.CacheReadTokens)
	}
	if total.Billing == nil || total.Billing.Cost == nil || *total.Billing.Cost != cost {
		t.Errorf("cost = %+v, want %+v", total.Billing, cost)
	}
}

func TestErrorKindClassifiesTheFailure(t *testing.T) {
	if got := errorKind(nil); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
	if got := errorKind(context.Canceled); got != "canceled" {
		t.Errorf("canceled = %q", got)
	}
	if got := errorKind(context.DeadlineExceeded); got != "deadline" {
		t.Errorf("deadline = %q", got)
	}
	if got := errorKind(errors.New("boom")); got != "error" {
		t.Errorf("other = %q", got)
	}
}
