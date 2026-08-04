package memorytest

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// buildRuntime is a small wrapper that handles the common case of
// "build a runtime from a spec + a set of impls, fail the test if
// construction errors". Tests can either pass a BuildRuntime
// function that does the full wiring, or use Build to share one
// spec across several suites.
func buildRuntime(t *testing.T, spec memory.Spec, impls memory.Impls) *memory.Runtime {
	t.Helper()
	rt, err := memory.New(spec, impls)
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// mustTextMessage builds a minimal message.Message that passes
// inference's own Validate. Useful when a suite needs a sample
// message but does not care about role or content shape.
func mustTextMessage(text string) message.Message {
	return message.Message{
		Role: message.RoleUser,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: text},
		}},
	}
}

// mustRecord wraps a message in a Record with the given ID. The
// record is what Append/Load/Recall share.
func mustRecord(id, text string) memory.Record {
	return memory.Record{ID: id, Message: mustTextMessage(text)}
}

// withCtx returns a context with the given timeout. Tests that
// need cancellation use it; the suite cleanup is the test's
// own responsibility.
func withCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
