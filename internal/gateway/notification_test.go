package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

type fakeOutboundChannel struct {
	name        string
	recipientID string
	message     string
	sendCalls   int
}

func (c *fakeOutboundChannel) Name() string { return c.name }

func (c *fakeOutboundChannel) ParseRequest(*http.Request) (*InboundMessage, *ChallengeResponse, error) {
	return nil, nil, nil
}

func (c *fakeOutboundChannel) FormatResponse(*workflow.Result) ([]byte, error) { return nil, nil }

func (c *fakeOutboundChannel) VerifySignature(*http.Request) error { return nil }

func (c *fakeOutboundChannel) Send(_ context.Context, recipientID string, message string) error {
	c.recipientID = recipientID
	c.message = message
	c.sendCalls++
	return nil
}

func newTestGatewayWithBinding(agentID string, ch Channel) *Gateway {
	router := &ChannelRouter{
		bindings: []resolvedBinding{
			{
				agentID: agentID,
				binding: model.ChannelBinding{Type: ch.Name()},
				channel: ch,
			},
		},
	}
	return &Gateway{router: router}
}

func TestNotificationRouter_Push_UsesTypedPayloadRuntimeID(t *testing.T) {
	ch := &fakeOutboundChannel{name: "out"}
	nr := &NotificationRouter{
		gateway: newTestGatewayWithBinding("builder", ch),
	}

	nr.push(context.Background(), "builder", ch.name, event.Event{
		Type: event.EventType(kanban.EventTaskCompleted),
		Payload: kanban.TaskCompletedPayload{
			CardID:        "card-1",
			TargetAgentID: "builder",
			RuntimeID:     "u-123",
			Output:        "done",
		},
		Timestamp: time.Now(),
	})

	if ch.sendCalls != 1 {
		t.Fatalf("sendCalls = %d, want 1", ch.sendCalls)
	}
	if ch.recipientID != "u-123" {
		t.Fatalf("recipientID = %q, want %q", ch.recipientID, "u-123")
	}
}

func TestNotificationRouter_PushCallbackSummary_ErrorFallsBackToFailureNotification(t *testing.T) {
	ch := &fakeOutboundChannel{name: "out"}
	nr := &NotificationRouter{
		gateway: newTestGatewayWithBinding("copilot", ch),
	}

	nr.pushCallbackSummary(context.Background(), "copilot", ch.name, event.Event{
		Type: event.EventType(kanban.EventCallbackDone),
		Payload: kanban.CallbackDonePayload{
			CardID:    "card-1",
			RuntimeID: "u-456",
			AgentID:   "copilot",
			Error:     "dispatcher failed",
		},
		Timestamp: time.Now(),
	})

	if ch.sendCalls != 1 {
		t.Fatalf("sendCalls = %d, want 1", ch.sendCalls)
	}
	if ch.recipientID != "u-456" {
		t.Fatalf("recipientID = %q, want %q", ch.recipientID, "u-456")
	}
	if ch.message != "📋 回调处理失败 (card: card-1): dispatcher failed" {
		t.Fatalf("message = %q", ch.message)
	}
}
