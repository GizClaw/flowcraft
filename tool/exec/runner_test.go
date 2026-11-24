package exec

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"github.com/GizClaw/flowcraft/sandbox/local"
)

// newRunnerIn builds the backend these tests drive: the no-isolation runner, which is the one backend
// that needs nothing from the host. The tools do not know which backend it is, and neither should
// their tests.
func newRunnerIn(t *testing.T) sandbox.Runner {
	t.Helper()
	runner, err := local.New(sandbox.Confine{})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	return runner
}

// bound is one call's authority: the session's process surface in the pocket, which is where a tool
// reads it from.
func bound(t *testing.T, runner sandbox.Runner) tool.Binding {
	t.Helper()
	set := tool.NewCapSet()
	platform.ProcessSlot.Provide(set, runner)
	return tool.Bind(testSession{}, policy.Grant{}).WithCaps(set)
}

type testSession struct{}

func (testSession) ID() string            { return "exec-test" }
func (testSession) Scratch() tool.Scratch { return testScratch{} }

type testScratch struct{}

func (testScratch) Load(string) (any, bool) { return nil, false }
func (testScratch) Store(string, any)       {}
func (testScratch) Delete(string)           {}

var _ = context.Background
