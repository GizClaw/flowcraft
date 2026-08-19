package inference

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func TestGenerateRequestRejectsStreamSourcesInContextAndInput(t *testing.T) {
	pipe := message.NewPartPipe(0)
	source, err := media.NewAudioStream[message.Part](pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	streamPart := message.AudioPart{Source: source}

	inContext := GenerateRequest{
		Context: []message.Message{{
			Role:    message.RoleUser,
			Content: message.Content{Parts: []message.Part{streamPart}},
		}},
	}
	if err := inContext.Validate(); err == nil ||
		!strings.Contains(err.Error(), "not allowed in context") {
		t.Fatalf("context Validate = %v, want context rejection", err)
	}

	inInput := GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{streamPart}},
				Intent:  Intent{Text: &TextIntent{}},
			},
		},
	}
	if err := inInput.Validate(); err == nil ||
		!strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("input Validate = %v, want input rejection", err)
	}
}
