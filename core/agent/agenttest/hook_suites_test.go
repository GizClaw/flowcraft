package agenttest_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// fakePreparer clones the previous board, satisfying the fresh-value
// and no-mutation rules.
type fakePreparer struct{}

func (fakePreparer) Before(
	_ context.Context,
	_ agent.Identity,
	_ *agent.Request,
	prev *agent.Board,
) (*agent.Board, error) {
	return prev.Clone(), nil
}

func TestPreparerSuite_FakePreparer(t *testing.T) {
	agenttest.PreparerSuite(t, func() agent.Preparer { return fakePreparer{} })
}

// fakeCommitter accepts everything.
type fakeCommitter struct{}

func (fakeCommitter) Commit(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
	return nil
}

func TestCommitterSuite_FakeCommitter(t *testing.T) {
	agenttest.CommitterSuite(t, func() agent.Committer { return fakeCommitter{} })
}

// fakeCommitViewProvider passes the result board through, rejecting
// results without one.
type fakeCommitViewProvider struct{}

func (fakeCommitViewProvider) CommitView(
	_ context.Context,
	_ agent.Identity,
	_ *agent.Request,
	res *agent.Result,
) (agent.CommitView, error) {
	if res == nil || res.LastBoard == nil {
		return agent.CommitView{}, errdefs.Validationf("agenttest: result board is required")
	}
	return agent.CommitView{LastBoard: res.LastBoard}, nil
}

func TestCommitViewProviderSuite_FakeProvider(t *testing.T) {
	agenttest.CommitViewProviderSuite(t, func() agent.CommitViewProvider { return fakeCommitViewProvider{} })
}

// fakeSuggester is a conforming advisory no-op.
type fakeSuggester struct{}

func (fakeSuggester) SuggestCheckpoint() error { return nil }

func TestCheckpointSuggesterSuite_FakeSuggester(t *testing.T) {
	agenttest.CheckpointSuggesterSuite(t, func() agent.CheckpointSuggester { return fakeSuggester{} })
}

// fakeScriptRuntime executes anything without error.
type fakeScriptRuntime struct{}

func (fakeScriptRuntime) Exec(
	context.Context,
	string,
	string,
	*agent.ScriptEnv,
) (*agent.ScriptSignal, error) {
	return nil, nil
}

func TestScriptRuntimeSuite_FakeRuntime(t *testing.T) {
	agenttest.ScriptRuntimeSuite(
		t,
		func() agent.ScriptRuntime { return fakeScriptRuntime{} },
		agenttest.ScriptFixture{Source: "1 + 1;"},
	)
}
