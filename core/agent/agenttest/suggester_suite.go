package agenttest

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
)

// SuggesterFactory builds a fresh [agent.CheckpointSuggester] for each
// subtest.
type SuggesterFactory func() agent.CheckpointSuggester

// CheckpointSuggesterSuite runs every contract probe that applies to
// the [agent.CheckpointSuggester] produced by f:
//
//   - a suggestion on a fresh engine must not panic and may return any
//     error (the host logs/retries; there is no classification
//     contract);
//   - SuggestCheckpoint is advisory and MUST return promptly — it is
//     not obligated to have written anything by the time it returns.
func CheckpointSuggesterSuite(t *testing.T, f SuggesterFactory) {
	t.Helper()

	t.Run("ZeroStateNoPanic", func(t *testing.T) {
		t.Helper()
		s := f()
		defer recoverPanicAs(t, "SuggestCheckpoint(fresh engine)")
		_ = s.SuggestCheckpoint()
	})
	t.Run("ReturnsPromptly", func(t *testing.T) {
		t.Helper()
		s := f()
		done := make(chan struct{})
		go func() {
			_ = s.SuggestCheckpoint()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("SuggestCheckpoint did not return within 2s; the suggestion is advisory and must return immediately")
		}
	})
}
