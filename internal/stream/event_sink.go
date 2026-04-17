package stream

import (
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// EventSink abstracts the transport used to push events to a client.
type EventSink interface {
	Send(ev MappedEvent) error
	Done(result *workflow.Result) error
	Error(code, message string) error
}
