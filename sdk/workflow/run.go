package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func (rt *runtime) run(ctx context.Context, agent Agent, req *Request, opts []RunOption) (*Result, error) {
	if req == nil {
		return nil, fmt.Errorf("workflow: nil request")
	}
	if agent == nil {
		return nil, fmt.Errorf("workflow: nil agent")
	}

	session, err := rt.openSession(ctx, agent, req.ContextID)
	if err != nil {
		return nil, err
	}
	var execErr error
	if session != nil {
		defer func() {
			_ = session.Close(ctx, execErr)
		}()
	}

	var board *Board
	if rt.prepareBoardFn != nil {
		board, err = rt.prepareBoardFn(ctx, agent, req, session, opts)
	} else {
		board, err = prepareBoard(ctx, req, session, opts)
	}
	if err != nil {
		execErr = err
		return nil, err
	}

	deps := rt.deps
	if deps == nil {
		deps = NewDependencies()
	}
	runnable, err := agent.Strategy().Build(ctx, deps)
	if err != nil {
		execErr = err
		return nil, err
	}

	board, execErr = runnable.Execute(ctx, board, req, opts...)
	return finishRun(ctx, agent, req, board, session, execErr)
}

func (rt *runtime) openSession(ctx context.Context, agent Agent, contextID string) (MemorySession, error) {
	if rt.memoryFactory == nil || contextID == "" {
		return nil, nil
	}
	mem, err := rt.memoryFactory(ctx, agent)
	if err != nil {
		return nil, err
	}
	if mem == nil {
		return nil, nil
	}
	return mem.Session(ctx, contextID)
}

func prepareBoard(ctx context.Context, req *Request, session MemorySession, opts []RunOption) (*Board, error) {
	board := NewBoard()
	rc := ApplyRunOpts(opts)

	prev := 0
	if session != nil {
		msgs, err := session.Load(ctx)
		if err != nil {
			return nil, fmt.Errorf("memory load: %w", err)
		}
		prev = len(msgs)
		cp := make([]model.Message, len(msgs))
		copy(cp, msgs)
		board.SetChannel(MainChannel, cp)
		sessionVars, err := session.Vars(ctx)
		if err != nil {
			return nil, fmt.Errorf("memory vars: %w", err)
		}
		for k, v := range sessionVars {
			board.SetVar(k, v)
		}
	} else if len(rc.History) > 0 {
		cp := make([]model.Message, len(rc.History))
		copy(cp, rc.History)
		board.SetChannel(MainChannel, cp)
		prev = len(cp)
	} else {
		board.SetChannel(MainChannel, []model.Message{})
	}

	board.AppendChannelMessage(MainChannel, req.Message)

	for k, v := range req.Inputs {
		board.SetVar(k, v)
	}

	q := MessageText(req.Message)
	if q != "" {
		board.SetVar(VarQuery, q)
	}
	if req.RuntimeID != "" {
		board.SetVar("runtime_id", req.RuntimeID)
	}
	runID := req.RunID
	if runID == "" {
		runID = genRunID()
	}
	board.SetVar(VarRunID, runID)

	board.SetVar(VarPrevMessageCount, prev)

	main := board.Channel(MainChannel)
	board.SetVar(VarMessages, append([]model.Message(nil), main...))

	return board, nil
}

func genRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b)
}

// finishRun builds the Result from execution outcome.
//
// Error semantics (W-5 fix): Run()'s returned error is reserved for
// infrastructure failures (e.g. memory save). All business terminal states
// (interrupted / canceled / aborted / failed) are expressed solely via
// Result.Status + Result.Err; Run() returns (res, nil) for these.
func finishRun(ctx context.Context, agent Agent, req *Request, board *Board, session MemorySession, execErr error) (*Result, error) {
	if board == nil {
		board = NewBoard()
	}
	res := &Result{
		TaskID:    req.TaskID,
		State:     make(map[string]any),
		LastBoard: board,
	}

	runID, _ := board.GetVar(VarRunID)
	res.State["run_id"] = runID

	if execErr != nil {
		res.Err = execErr
		switch {
		case errdefs.IsInterrupted(execErr):
			res.Status = StatusInterrupted
			res.State["board"] = board.Snapshot()
			if nodeID, ok := board.GetVar(VarInterruptedNode); ok {
				res.State["interrupted_node"] = nodeID
			}
		case errdefs.Is(execErr, context.Canceled),
			errdefs.Is(execErr, context.DeadlineExceeded):
			res.Status = StatusCanceled
		case errdefs.IsAborted(execErr):
			res.Status = StatusAborted
		default:
			res.Status = StatusFailed
		}
		return res, nil
	}

	prev := 0
	if pc, ok := board.GetVar(VarPrevMessageCount); ok {
		switch v := pc.(type) {
		case int:
			prev = v
		case int64:
			prev = int(v)
		}
	}

	main := board.Channel(MainChannel)
	if session != nil {
		if err := session.Save(ctx, main); err != nil {
			return nil, fmt.Errorf("memory save: %w", err)
		}
	}

	if prev > 0 && prev <= len(main) {
		res.Messages = append([]model.Message(nil), main[prev:]...)
	} else {
		res.Messages = append([]model.Message(nil), main...)
	}

	ak := agent.Strategy().Capabilities().AnswerVar()
	if v, ok := board.GetVar(ak); ok {
		res.State["answer"] = v
	}
	if u, ok := board.GetVar(VarInternalUsage); ok {
		if usage, ok := u.(model.TokenUsage); ok {
			res.Usage = usage
		}
	}
	res.Status = StatusCompleted
	return res, nil
}
