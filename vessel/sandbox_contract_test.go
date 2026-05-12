package vessel

// Internal-package contract test: assert vessel.sandboxHost satisfies
// the generic engine.Host contract via sdk/engine/hosttest. This is
// the second consumer of the suite (after engine.NoopHost in
// hosttest's own self-test) and the first non-trivial wrapper —
// sandboxHost embeds the caller-supplied host AND intercepts
// Checkpoint to stamp vessel.agent_name + persist via store. The
// suite catches any regression where one of the embedded methods
// stops being callable (e.g. an embedded NoopHost replaced by a
// pointer that nil-panics) or sandboxHost's overrides start
// panicking on zero-value inputs.
//
// Lives in `package vessel` (not vessel_test) because newSandboxHost
// is unexported. Internal contract tests are a recognised pattern
// in stdlib (see net/http/server_test.go's many internal tests
// against unexported handler types).

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/enginetest"
	"github.com/GizClaw/flowcraft/sdk/event"
)

func TestSandboxHost_Contract(t *testing.T) {
	enginetest.HostSuite(t, func() engine.Host {
		// A bus is required so Publish has somewhere to land;
		// engine.NoopHost is a safe inner host (its AskUser
		// returns NotAvailable, matching the suite's default
		// expectation).
		return newSandboxHost(engine.NoopHost{}, event.NewMemoryBus(), nil)
	})
}
