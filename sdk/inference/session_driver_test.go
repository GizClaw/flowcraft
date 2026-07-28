package inference

import (
	"context"
	"testing"
)

type canonicalRealtimeProviderSession struct{}

func (*canonicalRealtimeProviderSession) Send(context.Context, RealtimeInput) error {
	return nil
}

func (*canonicalRealtimeProviderSession) Next(
	context.Context,
) (RealtimeEvent, error) {
	return RealtimeTextDeltaEvent{Delta: "hello"}, nil
}

func (*canonicalRealtimeProviderSession) CancelResponse(context.Context) error {
	return nil
}

func (*canonicalRealtimeProviderSession) Close() error { return nil }

func TestBindRealtimeRejectsCanonicalSessionWire(t *testing.T) {
	_, err := BindRealtime(
		func(
			context.Context,
			ModelRef,
			RealtimeConfig,
		) (Compiled[string], error) {
			return Compiled[string]{}, nil
		},
		func(
			context.Context,
			string,
		) (ProviderRealtimeSession[RealtimeInput, RealtimeEvent], error) {
			return &canonicalRealtimeProviderSession{}, nil
		},
		func(
			context.Context,
			ModelRef,
			RealtimeInput,
		) (Compiled[RealtimeInput], error) {
			return Compiled[RealtimeInput]{}, nil
		},
		func(
			context.Context,
			RealtimeEvent,
		) (RealtimeEvent, error) {
			return RealtimeTextDeltaEvent{Delta: "hello"}, nil
		},
	)
	if err == nil {
		t.Fatal("BindRealtime accepted canonical input/event wire types")
	}
}
