package engine

import "github.com/GizClaw/flowcraft/sdk/inference"

// UserPrompt describes what the engine is asking the host to relay to
// the end user. It deliberately stays one level below "chat message":
//
//   - Parts carries the multi-modal payload (text, image, audio, file,
//     structured data) using inference.Part — the same building block
//     that inference.Message content uses, minus the chat-specific Role.
//   - Schema is an optional structured-input hint (JSON-schema-shaped
//     bytes) for cases where the host wants to render a form or
//     validate the response.
//   - Source identifies the engine step that produced the prompt;
//     useful for trace correlation and resume.
//   - Metadata is free-form host-passed-through metadata.
//
// Why []inference.Part rather than inference.Message: a Message also
// carries Role (system/user/assistant/tool), which is a chat-layer
// concept the engine has no business naming. Parts give us full
// multi-modality (image, audio, file, data) without tying the engine
// to chat semantics — the agent layer wraps Parts back into a Message
// with the right Role on its way out, and unwraps user-supplied Parts
// on the way in.
type UserPrompt struct {
	Parts    []inference.Part
	Schema   []byte
	Source   string
	Metadata map[string]string
}

// UserReply is what the host returns to satisfy a UserPrompt. Like
// UserPrompt it uses []inference.Part so the response can carry any
// modality the host's user interface produced — typed text, an
// uploaded image, recorded audio, a file attachment, structured form
// data, …
type UserReply struct {
	Parts    []inference.Part
	Metadata map[string]string
}
