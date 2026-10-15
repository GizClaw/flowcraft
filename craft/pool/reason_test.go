package pool

import (
	"context"
	"testing"
)

// TestReasonFromFallback pins what an untagged call reports: the reason
// is how a log line names the caller that asked for a replacement, and a
// caller that named none has to look the same whichever way it left the
// tag off.
func TestReasonFromFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "no context", ctx: nil, want: "unknown"},
		{name: "no tag", ctx: context.Background(), want: "unknown"},
		{name: "empty reason", ctx: WithReason(context.Background(), ""), want: "unknown"},
		{name: "tagged", ctx: WithReason(context.Background(), "settings_save"),
			want: "settings_save"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ReasonFrom(tc.ctx); got != tc.want {
				t.Fatalf("ReasonFrom = %q, want %q", got, tc.want)
			}
		})
	}
}
