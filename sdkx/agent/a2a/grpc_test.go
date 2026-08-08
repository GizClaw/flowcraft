package a2a_test

import (
	"context"
	"net"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdkx/agent/a2a"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// testA2AGRPCServer is a minimal in-memory A2A v1.0 gRPC service: it answers
// message/send with a completed task so the engine completes in one hop.
type testA2AGRPCServer struct {
	a2apb.UnimplementedA2AServiceServer
}

func (s *testA2AGRPCServer) SendMessage(context.Context, *a2apb.SendMessageRequest) (*a2apb.SendMessageResponse, error) {
	return &a2apb.SendMessageResponse{Payload: &a2apb.SendMessageResponse_Task{Task: completedGRPCTask()}}, nil
}

func (s *testA2AGRPCServer) GetTask(context.Context, *a2apb.GetTaskRequest) (*a2apb.Task, error) {
	return completedGRPCTask(), nil
}

func (s *testA2AGRPCServer) CancelTask(context.Context, *a2apb.CancelTaskRequest) (*a2apb.Task, error) {
	return completedGRPCTask(), nil
}

func completedGRPCTask() *a2apb.Task {
	return &a2apb.Task{
		Id:        "gt1",
		ContextId: "gc1",
		Status:    &a2apb.TaskStatus{State: a2apb.TaskState_TASK_STATE_COMPLETED},
		History: []*a2apb.Message{{
			MessageId: "gm1",
			Role:      a2apb.Role_ROLE_AGENT,
			Parts:     []*a2apb.Part{{Content: &a2apb.Part_Text{Text: "hello over gRPC"}}},
		}},
	}
}

// TestGRPCTransport verifies the engine reaches a remote agent over the
// gRPC binding: the transport is registered, selected from the card's GRPC
// interface, and the task lifecycle is transcribed onto the board.
func TestGRPCTransport(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	a2apb.RegisterA2AServiceServer(grpcServer, &testA2AGRPCServer{})
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	dialer := func(context.Context, string) (net.Conn, error) { return listener.Dial() }
	card := &a2aprotocol.AgentCard{
		Name:               "grpc-fake",
		Description:        "grpc fake for tests",
		Version:            "0.0.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		SupportedInterfaces: []*a2aprotocol.AgentInterface{{
			URL:             "passthrough:///bufconn",
			ProtocolBinding: a2aprotocol.TransportProtocolGRPC,
			ProtocolVersion: a2aprotocol.Version,
		}},
	}
	eng, err := a2a.New(context.Background(), card,
		a2a.WithGRPCDialOptions(
			grpc.WithContextDialer(dialer),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		),
	)
	if err != nil {
		t.Fatalf("a2a.New: %v", err)
	}

	res := runTurn(t, eng, "hi over grpc")
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %q err=%v, want completed", res.Status, res.Err)
	}
	if len(res.Messages) != 1 || !textEquals(res.Messages[0].Content.Parts[0], "hello over gRPC") {
		t.Errorf("messages = %#v, want the gRPC reply", res.Messages)
	}
	if got := res.LastBoard.GetVarString("a2a.task_id"); got != "gt1" {
		t.Errorf("a2a.task_id = %q, want gt1", got)
	}
}
