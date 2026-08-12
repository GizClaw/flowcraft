package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
)

// NewBoardBridge exposes board state as the global "board".
//
// Vars (control variables, untyped):
//   - getVar(key)      → any
//   - setVar(key, val)
//   - getVars()        → map[string]any
//   - hasVar(key)      → bool
//   - deleteVar(key)
//
// Channels (typed conversation history; multimodal-aware via the
// message.Message projection in project.go):
//   - channel(name)              → []messageMap   (read; never returns null)
//   - setChannel(name, msgs)     → throws on validation errors
//   - appendChannel(name, msg)   → throws on validation errors
//
// Constants:
//   - MAIN_CHANNEL — the engine's reserved default channel name; scripts
//     should reference this rather than hard-coding the literal string
//     so future renames do not break existing scripts.
//
// All channel APIs require an explicit name — scripts must opt into
// MainChannel by passing board.MAIN_CHANNEL themselves. This avoids
// accidentally stitching unrelated conversations together via an
// implicit default.
func NewBoardBridge(board *agent.Board) BindingFunc {
	return func(_ context.Context) (string, any) {
		return "board", map[string]any{
			"MAIN_CHANNEL": agent.MainChannel,

			"getVar":    func(key string) any { v, _ := board.GetVar(key); return v },
			"setVar":    func(key string, value any) { board.SetVar(key, value) },
			"deleteVar": func(key string) { board.DeleteVar(key) },
			"getVars":   func() map[string]any { return board.Vars() },
			"hasVar":    func(key string) bool { _, ok := board.GetVar(key); return ok },

			"channel": func(name string) ([]any, error) {
				return messagesToScript(board.Channel(name))
			},
			"setChannel": func(name string, raw any) error {
				msgs, err := messagesFromScript(raw, "setChannel")
				if err != nil {
					return err
				}
				board.SetChannel(name, msgs)
				return nil
			},
			"appendChannel": func(name string, raw any) error {
				msg, err := messageFromScript(raw, "appendChannel")
				if err != nil {
					return err
				}
				board.AppendChannelMessage(name, msg)
				return nil
			},
		}
	}
}
