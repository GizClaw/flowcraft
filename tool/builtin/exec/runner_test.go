package exec

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
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
func bound(t *testing.T, runner sandbox.SessionStarter) tool.Binding {
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

// askingStarter is a process surface that asks before it runs: the pre-flight the tool node calls.
type askingStarter struct {
	fakeStarter
	question string
	asked    []sandbox.SessionSpec
}

func (a *askingStarter) Ask(_ context.Context, spec sandbox.SessionSpec) (*message.Content, error) {
	a.asked = append(a.asked, spec)
	question := message.NewTextContent(a.question)
	return &question, nil
}

// TestThePreflightNamesTheCommandThatWouldRun pins the spec the parent judges to the command the
// tool would actually start: the whole leaf, and the directory it would run in. A pre-flight that
// described something else would be asking about the wrong command.
func TestThePreflightNamesTheCommandThatWouldRun(t *testing.T) {
	starter := &askingStarter{question: "may I?"}
	command := NewCommand(WithShell(testShell()))
	question, err := command.Ask(context.Background(), bound(t, starter), `{"command":"git push","workdir":"/srv"}`)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if question == nil || question.Text() != "may I?" {
		t.Fatalf("question = %v", question)
	}
	if len(starter.asked) != 1 {
		t.Fatalf("asked %d times, want 1", len(starter.asked))
	}
	argv := strings.Join(starter.asked[0].Argv, " ")
	if !strings.HasSuffix(argv, "git push") || !strings.HasPrefix(argv, command.shell.Path) {
		t.Fatalf("argv = %q, want the shell and the command line", argv)
	}
	if starter.asked[0].Opts.WorkDir != "/srv" {
		t.Fatalf("workdir = %q", starter.asked[0].Opts.WorkDir)
	}
}

// TestAPlainStarterIsNotAsked keeps the seam optional: a process surface with no approval rule has
// nothing to ask, and the call runs exactly as before.
func TestAPlainStarterIsNotAsked(t *testing.T) {
	command := NewCommand(WithShell(testShell()))
	question, err := command.Ask(context.Background(), bound(t, newFakeStarter()), `{"command":"echo hi","workdir":"/tmp"}`)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if question != nil {
		t.Fatalf("question = %q, want none", question.Text())
	}
}

// recordingStarter keeps both specs, so a test can compare the one the parent judges with the one
// that runs. The digest is computed from argv and workdir, so they have to be identical.
type recordingStarter struct {
	asked, started []sandbox.SessionSpec
}

func (r *recordingStarter) Ask(_ context.Context, spec sandbox.SessionSpec) (*message.Content, error) {
	r.asked = append(r.asked, spec)
	question := message.NewTextContent("may I?")
	return &question, nil
}

func (r *recordingStarter) Start(_ context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	r.started = append(r.started, spec)
	return nil, errdefs.NotAvailablef("not used")
}

// TestThePreflightSpecIsTheSpecThatRuns pins the identity the approval digest depends on: if the
// pre-flight describes a different argv or directory, the parent's answer never matches the command.
func TestThePreflightSpecIsTheSpecThatRuns(t *testing.T) {
	starter := &recordingStarter{}
	command := NewCommand(WithShell(testShell()))
	arguments := `{"command":"echo hi","workdir":"/srv"}`
	if _, err := command.Ask(context.Background(), bound(t, starter), arguments); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := command.Execute(context.Background(), bound(t, starter), arguments); err == nil {
		t.Fatal("the stub starter did not fail")
	}
	if len(starter.asked) != 1 || len(starter.started) != 1 {
		t.Fatalf("asked %d, started %d", len(starter.asked), len(starter.started))
	}
	if !slices.Equal(starter.asked[0].Argv, starter.started[0].Argv) {
		t.Fatalf("pre-flight argv %v != run argv %v", starter.asked[0].Argv, starter.started[0].Argv)
	}
	if starter.asked[0].Opts.WorkDir != starter.started[0].Opts.WorkDir {
		t.Fatalf("workdir %q != %q", starter.asked[0].Opts.WorkDir, starter.started[0].Opts.WorkDir)
	}
	if starter.asked[0].TTY != starter.started[0].TTY || starter.asked[0].Stdin != starter.started[0].Stdin {
		t.Fatalf("input side differs: asked %v/%v, started %v/%v",
			starter.asked[0].TTY, starter.asked[0].Stdin, starter.started[0].TTY, starter.started[0].Stdin)
	}
}
