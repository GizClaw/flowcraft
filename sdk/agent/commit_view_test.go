package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

func commitViewBoard(text, artifact string) *agent.Board {
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		inference.NewTextMessage(inference.RoleAssistant, text))
	board.AppendChannelMessage("summary",
		inference.NewTextMessage(inference.RoleAssistant, artifact))
	return board
}

func TestRun_CommitViewVisibleOnlyToCommitter(t *testing.T) {
	var order []string
	var providerResult *agent.Result
	var committedResult *agent.Result
	var observedResult *agent.Result

	referee := deciderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.Decision, error) {
		order = append(order, "referee")
		return agent.Decision{}, nil
	})
	provider := agent.CommitViewProviderFunc(func(_ context.Context, _ agent.Identity, _ *agent.Request, res *agent.Result) (agent.CommitView, error) {
		order = append(order, "provider")
		providerResult = res
		if !res.Committed || res.Text() != "original" {
			t.Fatalf("provider Result = committed %v text %q, want accepted original", res.Committed, res.Text())
		}
		return agent.CommitView{LastBoard: commitViewBoard("projected", "projected-artifact")}, nil
	})
	committer := agent.CommitterFunc(func(_ context.Context, _ agent.Identity, _ *agent.Request, res *agent.Result) error {
		order = append(order, "committer")
		committedResult = res
		if !res.Committed {
			t.Error("projected Result must be committed during Committer call")
		}
		return nil
	})
	observer := &endObserver{onEnd: func(res *agent.Result) {
		order = append(order, "observer")
		observedResult = res
	}}

	engine := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
		board.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "original"))
		board.AppendChannelMessage("summary",
			inference.NewTextMessage(inference.RoleAssistant, "original-artifact"))
		return board, nil
	})
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, engine, newReq("hi"),
		agent.WithArtifactChannels("summary"),
		agent.WithReferee(referee),
		agent.WithCommitViewProvider(provider),
		agent.WithCommitter(committer),
		agent.WithObserver(observer),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := strings.Join(order, ","), "referee,provider,committer,observer"; got != want {
		t.Fatalf("lifecycle order = %q, want %q", got, want)
	}
	if committedResult == nil || committedResult == res {
		t.Fatal("Committer must receive a projected Result copy")
	}
	if providerResult != res {
		t.Fatal("provider must inspect the original Result")
	}
	if got := committedResult.Text(); got != "projected" {
		t.Fatalf("Committer text = %q, want projected", got)
	}
	if committedResult.LastBoard == res.LastBoard {
		t.Fatal("Committer LastBoard must be the projected board")
	}
	if len(committedResult.Artifacts) != 1 ||
		committedResult.Artifacts[0].Name != "summary" ||
		len(committedResult.Artifacts[0].Parts) != 1 {
		t.Fatalf("Committer Artifacts = %+v, want materialized projected summary", committedResult.Artifacts)
	}
	if got := committedResult.Artifacts[0].Parts[0].(inference.TextPart).Text; got != "projected-artifact" {
		t.Fatalf("Committer artifact text = %q, want projected-artifact", got)
	}
	if got := res.Text(); got != "original" {
		t.Fatalf("returned Result text = %q, want original", got)
	}
	if len(res.Artifacts) != 1 ||
		res.Artifacts[0].Parts[0].(inference.TextPart).Text != "original-artifact" {
		t.Fatalf("returned Artifacts = %+v, want original engine artifact", res.Artifacts)
	}
	if observedResult != res || observedResult.Text() != "original" {
		t.Fatalf("Observer Result = %p text %q, want original returned Result %p", observedResult, observedResult.Text(), res)
	}
	if !res.Committed {
		t.Fatal("successful Committer must leave original Result committed")
	}
}

func TestRun_CommitViewProviderSkippedForDiscardAndNoCommitter(t *testing.T) {
	tests := map[string]struct {
		opts []agent.ExecuteOption
	}{
		"discard": {
			opts: []agent.ExecuteOption{
				agent.WithReferee(deciderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.Decision, error) {
					return agent.Decision{DiscardOutput: true}, nil
				})),
				agent.WithCommitter(agent.CommitterFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
					t.Fatal("Committer called for discarded Result")
					return nil
				})),
			},
		},
		"no committer": {},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			providerCalls := 0
			opts := append([]agent.ExecuteOption{
				agent.WithCommitViewProvider(agent.CommitViewProviderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.CommitView, error) {
					providerCalls++
					return agent.CommitView{LastBoard: commitViewBoard("projected", "")}, nil
				})),
			}, tc.opts...)
			if _, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("original"), newReq("hi"), opts...); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if providerCalls != 0 {
				t.Fatalf("provider calls = %d, want 0", providerCalls)
			}
		})
	}
}

func TestRun_CommitViewFailureSkipsCommitterAndPreservesOriginal(t *testing.T) {
	tests := map[string]struct {
		view    agent.CommitView
		err     error
		wantErr func(error) bool
	}{
		"provider error": {
			err:     errdefs.NotAvailable(errors.New("projection unavailable")),
			wantErr: errdefs.IsNotAvailable,
		},
		"nil board": {
			wantErr: errdefs.IsValidation,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			committerCalls := 0
			var observed *agent.Result
			res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("original"), newReq("hi"),
				agent.WithCommitViewProvider(agent.CommitViewProviderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.CommitView, error) {
					return tc.view, tc.err
				})),
				agent.WithCommitter(agent.CommitterFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
					committerCalls++
					return nil
				})),
				agent.WithObserver(&endObserver{onEnd: func(r *agent.Result) { observed = r }}),
			)
			if err == nil || !tc.wantErr(err) {
				t.Fatalf("Execute error = %v, want preserved classification", err)
			}
			if committerCalls != 0 {
				t.Fatalf("Committer calls = %d, want 0", committerCalls)
			}
			if res == nil || res.Text() != "original" || res.Committed {
				t.Fatalf("returned Result = %+v, want original output and Committed=false", res)
			}
			if got := res.State["finalize_reason"]; got != "commit_view_failed" {
				t.Fatalf("finalize_reason = %v, want commit_view_failed", got)
			}
			if observed != res {
				t.Fatal("Observer must receive the original failed Result")
			}
		})
	}
}

func TestRun_CommitViewProviderLastWins(t *testing.T) {
	firstCalls := 0
	var got string
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("original"), newReq("hi"),
		agent.WithCommitViewProvider(agent.CommitViewProviderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.CommitView, error) {
			firstCalls++
			return agent.CommitView{LastBoard: commitViewBoard("first", "")}, nil
		})),
		agent.WithCommitViewProvider(agent.CommitViewProviderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.CommitView, error) {
			return agent.CommitView{LastBoard: commitViewBoard("last", "")}, nil
		})),
		agent.WithCommitter(agent.CommitterFunc(func(_ context.Context, _ agent.Identity, _ *agent.Request, res *agent.Result) error {
			got = res.Text()
			return nil
		})),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if firstCalls != 0 || got != "last" {
		t.Fatalf("first calls = %d, Committer text = %q; want 0 and last", firstCalls, got)
	}
	if res.Text() != "original" {
		t.Fatalf("returned text = %q, want original", res.Text())
	}
}

func TestRun_CommitViewProviderTypedNilIsIgnored(t *testing.T) {
	var provider agent.CommitViewProviderFunc
	var got *agent.Result
	firstCalls := 0
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("original"), newReq("hi"),
		agent.WithCommitViewProvider(agent.CommitViewProviderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.CommitView, error) {
			firstCalls++
			return agent.CommitView{LastBoard: commitViewBoard("first", "")}, nil
		})),
		agent.WithCommitViewProvider(provider),
		agent.WithCommitter(agent.CommitterFunc(func(_ context.Context, _ agent.Identity, _ *agent.Request, res *agent.Result) error {
			got = res
			return nil
		})),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if firstCalls != 0 || got != res {
		t.Fatal("last typed-nil provider should disable an earlier provider")
	}
}
