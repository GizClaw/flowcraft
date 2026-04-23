package webrtc

import (
	"fmt"

	"github.com/GizClaw/flowcraft/voice"
	"github.com/GizClaw/flowcraft/voice/audio"
)

type MessageType string

const (
	MessageStart     MessageType = "start"
	MessageAudio     MessageType = "audio"
	MessageText      MessageType = "text"
	MessageCommit    MessageType = "commit"
	MessageInterrupt MessageType = "interrupt"
	MessageClose     MessageType = "close"
	MessageEvent     MessageType = "event"
	MessageError     MessageType = "error"
)

type CloseReason string

const (
	CloseReasonClientEnd   CloseReason = "client_end"
	CloseReasonServerEnd   CloseReason = "server_end"
	CloseReasonIdleTimeout CloseReason = "idle_timeout"
	CloseReasonProtocol    CloseReason = "protocol_error"
)

type ClientMessage struct {
	Type      MessageType   `json:"type"`
	Start     *StartPayload `json:"start,omitempty"`
	Audio     *AudioPayload `json:"audio,omitempty"`
	Text      *TextPayload  `json:"text,omitempty"`
	Close     *ClosePayload `json:"close,omitempty"`
	RequestID string        `json:"request_id,omitempty"`
}

type ServerMessage struct {
	Type      MessageType   `json:"type"`
	Event     *EventPayload `json:"event,omitempty"`
	Error     *ErrorPayload `json:"error,omitempty"`
	Close     *ClosePayload `json:"close,omitempty"`
	RequestID string        `json:"request_id,omitempty"`
}

type StartPayload struct {
	Capabilities    voice.SessionCapabilities `json:"capabilities"`
	InputFormat     audio.Format               `json:"input_format"`
	OutputFormat    audio.Format               `json:"output_format"`
	AcceptedCodecs  []audio.Codec              `json:"accepted_codecs,omitempty"`
	PreferredCodecs []audio.Codec              `json:"preferred_codecs,omitempty"`
}

type AudioPayload struct {
	Frame audio.Frame `json:"frame"`
}

type TextPayload struct {
	Text string `json:"text"`
}

type ClosePayload struct {
	Reason CloseReason `json:"reason"`
	Code   string      `json:"code,omitempty"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type EventPayload struct {
	Event voice.Event `json:"event"`
}

func SessionOptionsFromStart(start *StartPayload) []voice.SessionOption {
	if start == nil {
		return nil
	}
	return []voice.SessionOption{
		voice.WithCapabilities(start.Capabilities),
	}
}

func ApplyControlMessage(session *voice.Session, msg ClientMessage) (bool, error) {
	if session == nil {
		return false, fmt.Errorf("speech/ws: nil session")
	}
	switch msg.Type {
	case MessageCommit:
		return session.CommitInput(), nil
	case MessageInterrupt:
		return session.StopSpeaking(), nil
	case MessageText:
		if msg.Text == nil {
			return false, fmt.Errorf("speech/ws: text payload is required")
		}
		return session.Send(msg.Text.Text), nil
	case MessageClose:
		return true, nil
	default:
		return false, fmt.Errorf("speech/ws: unsupported control message %q", msg.Type)
	}
}

func EventMessage(ev voice.Event, requestID string) ServerMessage {
	return ServerMessage{
		Type:      MessageEvent,
		Event:     &EventPayload{Event: ev},
		RequestID: requestID,
	}
}

func ErrorMessage(code, message, requestID string) ServerMessage {
	return ServerMessage{
		Type: MessageError,
		Error: &ErrorPayload{
			Code:    code,
			Message: message,
		},
		RequestID: requestID,
	}
}

func CloseMessage(reason CloseReason, code, requestID string) ServerMessage {
	return ServerMessage{
		Type: MessageClose,
		Close: &ClosePayload{
			Reason: reason,
			Code:   code,
		},
		RequestID: requestID,
	}
}
