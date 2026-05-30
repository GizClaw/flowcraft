package background

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Outcome
	}{
		{name: "nil", err: nil, want: OutcomeSucceeded},
		{name: "timeout", err: context.DeadlineExceeded, want: OutcomeTimeout},
		{name: "wrapped timeout", err: fmt.Errorf("work: %w", context.DeadlineExceeded), want: OutcomeTimeout},
		{name: "canceled", err: context.Canceled, want: OutcomeCanceled},
		{name: "wrapped canceled", err: fmt.Errorf("work: %w", context.Canceled), want: OutcomeCanceled},
		{name: "failed", err: errors.New("boom"), want: OutcomeFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Fatalf("Classify() = %q, want %q", got, tt.want)
			}
			if tt.want.String() == "" {
				t.Fatal("Outcome.String returned empty label")
			}
		})
	}
}
