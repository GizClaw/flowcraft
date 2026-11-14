package inference

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// DigestMessage is a message's canonical identity and content size.
//
// It hashes the structured message instead of its JSON form: the JSON encoder
// validates and normalizes every part, which for a long transcript costs more
// than the model call it identifies.
func DigestMessage(msg message.Message) (string, int) {
	h := sha256.New()
	writeText(h, "role", string(msg.Role))
	chars := len(msg.Role)
	if msg.Metadata != nil {
		if body, err := json.Marshal(msg.Metadata); err == nil {
			writeText(h, "metadata", string(body))
			chars += len(body)
		}
	}
	chars += digestParts(h, msg.Content.Parts)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), chars
}

// MessageHash returns only the identity.
func MessageHash(msg message.Message) string {
	hash, _ := DigestMessage(msg)
	return hash
}

func digestParts(h hash.Hash, parts []message.Part) int {
	chars := 0
	for _, part := range parts {
		switch typed := part.(type) {
		case message.TextPart:
			writeText(h, "text", typed.Text)
			chars += len(typed.Text)
		case message.ReasoningPart:
			writeText(h, "reasoning", typed.Text)
			writeText(h, "signature", typed.Signature)
			writeText(h, "reasoning-id", typed.ID)
			chars += len(typed.Text)
		case message.ToolCallPart:
			writeText(h, "call", typed.Call.ID)
			writeText(h, "tool", typed.Call.Name)
			args := typed.Call.Arguments.Raw()
			writeText(h, "args", string(args))
			chars += len(args)
		case message.ToolResultPart:
			writeText(h, "result-call", typed.Result.CallID)
			writeText(h, "is-error", strconv.FormatBool(typed.Result.IsError))
			chars += digestParts(h, typed.Result.Content.Parts)
		case message.ImagePart:
			chars += digestSource(h, "image", string(typed.Source.Kind()), typed.Source.URL(),
				typed.Source.MediaType(), typed.Source.Bytes())
		case message.AudioPart:
			chars += digestSource(h, "audio", string(typed.Source.Kind()), typed.Source.URL(),
				typed.Source.MediaType(), typed.Source.Bytes())
			writeText(h, "encoding", string(typed.Format.Encoding))
			writeText(h, "sample-rate", strconv.Itoa(typed.Format.SampleRateHz))
			writeText(h, "channels", strconv.Itoa(typed.Format.Channels))
			writeText(h, "duration", strconv.FormatInt(typed.DurationMillis, 10))
		case message.VideoPart:
			chars += digestSource(h, "video", string(typed.Source.Kind()), typed.Source.URL(),
				typed.Source.MediaType(), typed.Source.Bytes())
		case message.FilePart:
			chars += digestSource(h, "file", string(typed.Source.Kind()), typed.Source.URL(),
				typed.Source.MediaType(), typed.Source.Bytes())
			writeText(h, "name", typed.Name)
		default:
			// A part kind this build does not know: its wire form is the only
			// stable description left.
			if body, err := json.Marshal(part); err == nil {
				writeText(h, "part", string(body))
				chars += len(body)
			}
		}
	}
	return chars
}

func digestSource(h hash.Hash, label, kind, url, mediaType string, data []byte) int {
	writeText(h, "source-kind", kind)
	writeText(h, "url", url)
	writeText(h, "media-type", mediaType)
	writeText(h, "data", string(data))
	return len(data)
}
