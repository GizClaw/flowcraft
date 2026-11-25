package platform

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Questions is how a tool asks the conversation's user something and later reads the answer.
//
// The ask is a pre-flight, not a call that blocks: [Questions.Ask] reports the question the turn
// should stop on, the host records it, and the answer arrives as the session's next resume. That is
// what keeps a turn from holding its slot for as long as a person takes — the reason a blocking ask
// cannot work for a delegated child, whose parent could not answer while its turn was held.
//
// It is a capability slot rather than a host reference for the same reason every other slot is: a
// tool may not reach the host. The two calls are separate because they happen on two turns — Ask
// before the suspension, Take after the resume — and the answer belongs to the session in between.
type Questions interface {
	// Ask records the question and reports the text the turn suspends on, or nil when the session
	// already holds an answer and the call may run.
	Ask(ctx context.Context, question message.Content) *message.Content
	// Take consumes the answer the session is holding, once.
	Take(ctx context.Context) (message.Content, bool)
}

// AskSlot is the capability a tool declares in [tool.Tool.Needs] to ask the user.
var AskSlot = tool.NewSlot[Questions]("tool.questions")
