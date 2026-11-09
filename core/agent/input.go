package agent

import "github.com/GizClaw/flowcraft/core/message"

// Reply is the answer to a question a Task paused on.
//
// It is Content, not a string: the protocol delivers parts, and flattening them
// to text silently drops an image or a file a client sent as the answer. A reply
// always comes from the user, so it carries no role and no message list.
type Reply struct{ Content message.Content }

// Input is one user request.
type Input struct {
	// System frames the request. It is Content, not a string, because a system
	// prompt is a list of parts like any other message.
	System message.Content
	// Messages are the new turns to add before answering.
	Messages []message.Message
}
