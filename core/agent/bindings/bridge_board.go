package bindings

import (
	"context"
	"encoding/json"
	"fmt"

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
//   - resolve(str)         → any       (typed ${board:*} expansion)
//   - resolveString(str)   → string    (text ${board:*} expansion)
//
// Channels (typed conversation history; multimodal-aware via the
// message.Message projection in project.go):
//   - channel(name)              → []messageMap   (read; never returns null)
//   - channelLen(name)           → number         (length; copies nothing)
//   - lastMessage(name)          → messageMap | null (tail; copies one message)
//   - channelTail(name, count)   → []messageMap   (last count, in order)
//   - setChannel(name, msgs)     → throws on validation errors
//   - appendChannel(name, msg)   → throws on validation errors; msg is one
//     message object or an array of them (appended as one batch)
//
// The narrow reads exist because channel() projects every message into a
// fresh object graph: a script that only needs a length or a tail of a
// long conversation should not pay for the whole transcript. They read the
// same channel through the same projection, so a message is never shown in
// two shapes.
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

			"resolve": func(s string) (any, error) {
				return board.ResolveString(s)
			},
			"resolveString": func(s string) (string, error) {
				v, err := board.ResolveString(s)
				if err != nil {
					return "", err
				}
				if str, ok := v.(string); ok {
					return str, nil
				}
				if b, err := json.Marshal(v); err == nil {
					return string(b), nil
				}
				return fmt.Sprintf("%v", v), nil
			},

			"channel": func(name string) ([]any, error) {
				return messagesToScript(board.ChannelView(name))
			},
			"channelLen": func(name string) int {
				return board.ChannelLen(name)
			},
			"lastMessage": func(name string) (any, error) {
				msg, ok := board.LastMessage(name)
				if !ok {
					return nil, nil
				}
				mm, err := messageToScript(msg, "lastMessage")
				if err != nil {
					return nil, err
				}
				return mm, nil
			},
			"channelTail": func(name string, count int) ([]any, error) {
				return messagesToScript(board.ChannelTail(name, count))
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
				msgs, err := appendMessagesFromScript(raw, "appendChannel")
				if err != nil {
					return err
				}
				board.AppendChannelMessages(name, msgs)
				return nil
			},
		}
	}
}
