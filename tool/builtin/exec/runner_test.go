package exec

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

// newRunnerIn is the backend these tests drive: a starter of this package's own that runs a real
// process (see fake_test.go). Keeping it here is what lets the module avoid depending on a backend
// module just to test a tool.
func newRunnerIn(t *testing.T) *fakeStarter {
	t.Helper()
	return newFakeStarter()
}

// bound is one call's authority: the session's process surface in the pocket, which is where a tool
// reads it from.
func bound(t *testing.T, runner *fakeStarter) tool.Binding {
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
