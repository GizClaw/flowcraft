package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// Board is the structural contract NewBoardBridge requires of any
// blackboard-shaped object. *engine.Board satisfies it directly; the
// legacy *workflow.Board also fits the same shape, which is what lets
// existing callers keep compiling during the v0.3.0 transition without
// the bridge taking on a host dependency.
//
// Method set rationale:
//   - GetVar / SetVar / Vars: plain control-variable access.
//   - Channel / SetChannel / AppendChannelMessage: typed message
//     channels — needed because LLM rounds keep multimodal
//     conversation history in channels, not vars. Scripts need the
//     ability to read the current history, replace it after a round
//     (setChannel with r.messages), and append individual user /
//     assistant turns (appendChannel).
type Board interface {
	GetVar(key string) (any, bool)
	SetVar(key string, value any)
	Vars() map[string]any
	Channel(name string) []model.Message
	SetChannel(name string, msgs []model.Message)
	AppendChannelMessage(name string, msg model.Message)
}

// NewBoardBridge exposes board state as the global "board".
//
// Vars (control variables, untyped):
//   - getVar(key)      → any
//   - setVar(key, val)
//   - getVars()        → map[string]any
//   - hasVar(key)      → bool
//
// Channels (typed conversation history; multimodal-aware via the
// model.Message projection in bridge_llm_marshal.go):
//   - channel(name)              → []messageMap   (read; never returns null)
//   - setChannel(name, msgs)     → throws on validation errors
//   - appendChannel(name, msg)   → throws on validation errors
//
// All channel APIs require an explicit name — scripts must opt into
// MainChannel by passing "" themselves. This avoids accidentally
// stitching unrelated conversations together via an implicit default.
func NewBoardBridge(board Board) BindingFunc {
	return func(_ context.Context) (string, any) {
		return "board", map[string]any{
			"getVar":  func(key string) any { v, _ := board.GetVar(key); return v },
			"setVar":  func(key string, value any) { board.SetVar(key, value) },
			"getVars": func() map[string]any { return board.Vars() },
			"hasVar":  func(key string) bool { _, ok := board.GetVar(key); return ok },

			"channel": func(name string) []map[string]any {
				return messagesToList(board.Channel(name))
			},
			"setChannel": func(name string, raw any) error {
				msgs, err := parseChannelMessages(raw, "setChannel")
				if err != nil {
					return err
				}
				board.SetChannel(name, msgs)
				return nil
			},
			"appendChannel": func(name string, raw any) error {
				msg, err := parseMessage(raw, "appendChannel")
				if err != nil {
					return err
				}
				board.AppendChannelMessage(name, msg)
				return nil
			},
		}
	}
}
