package nodes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// recordLLMCall is the record kind a model call writes.
const recordLLMCall = "llm.call"

// describePrompt renders the request's input identity: one digest per message
// plus the roll-up segments a dashboard reads.
//
// The digest is what makes a failure attributable to one retrieved chunk
// instead of "the context changed"; the roll-up is a dimension, not a key.
func describePrompt(request inference.GenerateRequest) ([]agent.MessageDigest, []agent.PromptSegment) {
	digests := make([]agent.MessageDigest, 0, len(request.Context)+1)

	var system, context []agent.MessageDigest
	for i, msg := range request.Context {
		digest := digestOf(msg)
		digests = append(digests, digest)
		if i == 0 && msg.Role == message.RoleSystem {
			system = append(system, digest)
			continue
		}
		context = append(context, digest)
	}
	input := digestOf(message.Message{
		Role:    inputRoleOf(request.Input.Role),
		Content: request.Input.Content,
	})
	digests = append(digests, input)

	segments := make([]agent.PromptSegment, 0, 5)
	segments = append(segments, messageSegment("system", system)...)
	segments = append(segments, messageSegment("context", context)...)
	segments = append(segments, messageSegment("input", []agent.MessageDigest{input})...)

	names := make([]string, 0, len(request.Tools.Definitions()))
	for _, definition := range request.Tools.Definitions() {
		names = append(names, definition.Name)
	}
	sort.Strings(names)
	choice, _ := json.Marshal(request.Tools.Choice())
	segments = append(segments, rawSegment("tools", append([]byte(joinNames(names)), choice...)))

	fields := request.Extensions.ActiveFields()
	fieldNames := make([]string, 0, len(fields))
	for _, field := range fields {
		fieldNames = append(fieldNames, string(field))
	}
	sort.Strings(fieldNames)
	segments = append(segments, rawSegment("extensions", []byte(joinNames(fieldNames))))
	return digests, segments
}

// digestOf renders one message's identity. It uses the same structural digest
// the request hash uses, so a call pays one pass over the transcript.
func digestOf(msg message.Message) agent.MessageDigest {
	hash, chars := inference.DigestMessage(msg)
	return agent.MessageDigest{
		Role:   msg.Role,
		Origin: originOf(msg),
		Chars:  chars,
		Hash:   hash,
	}
}

// originOf is the provenance tag the producer attached, or the role's default.
func originOf(msg message.Message) string {
	if msg.Metadata != nil {
		if raw, ok := msg.Metadata.Attributes["origin"]; ok {
			var origin string
			if err := json.Unmarshal(raw, &origin); err == nil && origin != "" {
				return origin
			}
		}
	}
	switch msg.Role {
	case message.RoleUser:
		return "user"
	case message.RoleAssistant:
		return "assistant"
	case message.RoleSystem:
		return "system"
	case message.RoleTool:
		return "tool"
	}
	return string(msg.Role)
}

// inputRoleOf maps the input role back to a message role for digesting.
func inputRoleOf(role inference.InputRole) message.Role {
	if role == inference.InputRoleTool {
		return message.RoleTool
	}
	return message.RoleUser
}

// messageSegment folds message digests into one segment hash.
func messageSegment(id string, digests []agent.MessageDigest) []agent.PromptSegment {
	if len(digests) == 0 {
		return nil
	}
	hash := sha256.New()
	chars := 0
	for _, digest := range digests {
		hash.Write([]byte(digest.Hash))
		chars += digest.Chars
	}
	return []agent.PromptSegment{{
		ID:    id,
		Chars: chars,
		Hash:  "sha256:" + hex.EncodeToString(hash.Sum(nil)),
	}}
}

// rawSegment folds canonical bytes into one segment hash.
func rawSegment(id string, body []byte) agent.PromptSegment {
	sum := sha256.Sum256(body)
	return agent.PromptSegment{
		ID:    id,
		Chars: len(body),
		Hash:  "sha256:" + hex.EncodeToString(sum[:]),
	}
}

func joinNames(names []string) string {
	body := make([]byte, 0, len(names)*8)
	for i, name := range names {
		if i > 0 {
			body = append(body, '\n')
		}
		body = append(body, name...)
	}
	return string(body)
}
