package agent

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

// BoardSeeder builds the initial [engine.Board] for a run.
//
// It is the single extension point for "anything that should be on
// the board before the engine sees it":
//
//   - conversation history (load from sdk/history, summarise, window);
//   - retrieved long-term memory (sdk/recall results, knowledge-base
//     hits);
//   - system prompts and persona text;
//   - request-scoped board vars (form fields, parameters, tool
//     allow-lists);
//   - any combination of the above.
//
// Run guarantees:
//
//   - SeedBoard is called exactly once per Run, before
//     engine.Execute and before any Observer's OnRunStart.
//   - The returned board is mutated by the engine; SeedBoard must
//     therefore return a fresh value each call (do NOT cache and
//     re-yield a single Board).
//   - The returned board MUST be non-nil. Returning nil is a Run
//     infrastructure error.
//
// Implementations are expected to be cheap and synchronous; long
// async work (retrieval, IO) belongs in a wrapper that resolves
// before Run.
type BoardSeeder interface {
	SeedBoard(ctx context.Context, info RunInfo, req *Request) (*engine.Board, error)
}

// BoardSeederFunc is the function-typed adapter for BoardSeeder.
//
// Useful when the seed logic is a single closure over a transcript
// loader or retriever:
//
//	agent.WithBoardSeed(agent.BoardSeederFunc(func(ctx context.Context, info agent.RunInfo, req *agent.Request) (*engine.Board, error) {
//	    prior, err := store.Load(ctx, info.ContextID)
//	    if err != nil { return nil, err }
//	    b := engine.NewBoard()
//	    b.SetChannel(engine.MainChannel, prior)
//	    b.AppendChannelMessage(engine.MainChannel, req.Message)
//	    return b, nil
//	}))
type BoardSeederFunc func(ctx context.Context, info RunInfo, req *Request) (*engine.Board, error)

// SeedBoard calls f.
func (f BoardSeederFunc) SeedBoard(ctx context.Context, info RunInfo, req *Request) (*engine.Board, error) {
	return f(ctx, info, req)
}

// defaultSeeder is the seed Run uses when [WithBoardSeed] is not
// configured. It produces a fresh board, appends req.Message to
// MainChannel, and copies req.Inputs into board vars. It does NOT
// load any history; that is a deliberate choice — agents that need
// transcript continuity wire it through a custom BoardSeeder (most
// often a thin closure around sdk/history).
type defaultSeeder struct{}

// SeedBoard implements [BoardSeeder].
func (defaultSeeder) SeedBoard(_ context.Context, _ RunInfo, req *Request) (*engine.Board, error) {
	b := engine.NewBoard()
	b.AppendChannelMessage(engine.MainChannel, req.Message)
	for k, v := range req.Inputs {
		b.SetVar(k, v)
	}
	return b, nil
}

var _ BoardSeeder = defaultSeeder{}
var _ BoardSeeder = BoardSeederFunc(nil)
