package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/plugin"
	pb "github.com/GizClaw/flowcraft/plugin/proto"
)

// grpcToolClient implements ToolServiceClient over gRPC.
type grpcToolClient struct {
	client pb.ToolServiceClient
}

func (c *grpcToolClient) ListTools(ctx context.Context) ([]plugin.ToolSpec, error) {
	resp, err := c.client.ListTools(ctx, &pb.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("grpc: list tools: %w", err)
	}
	return toolSpecsFromProto(resp.Tools), nil
}

func (c *grpcToolClient) Execute(ctx context.Context, name string, arguments string) (string, error) {
	resp, err := c.client.Execute(ctx, &pb.ToolExecuteRequest{
		Name:      name,
		Arguments: arguments,
	})
	if err != nil {
		return "", fmt.Errorf("grpc: execute tool %q: %w", name, err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("grpc: tool %q: %s", name, resp.Error)
	}
	return resp.Result, nil
}

// grpcNodeClient implements NodeServiceClient over gRPC bidirectional streaming.
type grpcNodeClient struct {
	client pb.NodeServiceClient
}

func (c *grpcNodeClient) ListNodes(ctx context.Context) ([]plugin.NodeSpec, error) {
	resp, err := c.client.ListNodes(ctx, &pb.ListNodesRequest{})
	if err != nil {
		return nil, fmt.Errorf("grpc: list nodes: %w", err)
	}
	return nodeSpecsFromProto(resp.Nodes), nil
}

func (c *grpcNodeClient) Execute(ctx context.Context, nodeID string, config map[string]any, callbacks plugin.NodeCallbacks) (map[string]any, error) {
	stream, err := c.client.Execute(ctx)
	if err != nil {
		return nil, fmt.Errorf("grpc: open node stream: %w", err)
	}

	configMap := make(map[string]string, len(config))
	for k, v := range config {
		b, _ := json.Marshal(v)
		configMap[k] = string(b)
	}

	if err := stream.Send(&pb.NodeMessage{
		Msg: &pb.NodeMessage_ExecuteRequest{
			ExecuteRequest: &pb.NodeExecuteRequest{
				NodeId: nodeID,
				Config: configMap,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("grpc: send execute request: %w", err)
	}

	// Process callback loop until we get the final response
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil, fmt.Errorf("grpc: stream ended without response")
		}
		if err != nil {
			return nil, fmt.Errorf("grpc: recv: %w", err)
		}

		switch m := msg.Msg.(type) {
		case *pb.NodeMessage_ExecuteResponse:
			if m.ExecuteResponse.Error != "" {
				return nil, fmt.Errorf("grpc: node %q: %s", nodeID, m.ExecuteResponse.Error)
			}
			result := make(map[string]any, len(m.ExecuteResponse.Outputs))
			for k, v := range m.ExecuteResponse.Outputs {
				var decoded any
				if err := json.Unmarshal([]byte(v), &decoded); err != nil {
					result[k] = v
				} else {
					result[k] = decoded
				}
			}
			return result, nil

		case *pb.NodeMessage_GetVarRequest:
			val, found := callbacks.GetVar(m.GetVarRequest.Key)
			valStr := ""
			if found {
				b, _ := json.Marshal(val)
				valStr = string(b)
			}
			if err := stream.Send(&pb.NodeMessage{
				Msg: &pb.NodeMessage_GetVarResponse{
					GetVarResponse: &pb.GetVarResponse{
						Value: valStr,
						Found: found,
					},
				},
			}); err != nil {
				return nil, fmt.Errorf("grpc: send get_var response: %w", err)
			}

		case *pb.NodeMessage_SetVarRequest:
			var decoded any
			if err := json.Unmarshal([]byte(m.SetVarRequest.Value), &decoded); err != nil {
				callbacks.SetVar(m.SetVarRequest.Key, m.SetVarRequest.Value)
			} else {
				callbacks.SetVar(m.SetVarRequest.Key, decoded)
			}

		case *pb.NodeMessage_LlmGenerateRequest:
			result, genErr := callbacks.LLMGenerate(ctx, m.LlmGenerateRequest.Prompt)
			errStr := ""
			if genErr != nil {
				errStr = genErr.Error()
			}
			if err := stream.Send(&pb.NodeMessage{
				Msg: &pb.NodeMessage_LlmGenerateResponse{
					LlmGenerateResponse: &pb.LLMGenerateResponse{
						Result: result,
						Error:  errStr,
					},
				},
			}); err != nil {
				return nil, fmt.Errorf("grpc: send llm_generate response: %w", err)
			}

		case *pb.NodeMessage_ToolExecuteRequest:
			result, toolErr := callbacks.ToolExecute(ctx, m.ToolExecuteRequest.Name, m.ToolExecuteRequest.Arguments)
			errStr := ""
			if toolErr != nil {
				errStr = toolErr.Error()
			}
			if err := stream.Send(&pb.NodeMessage{
				Msg: &pb.NodeMessage_ToolExecuteResponse{
					ToolExecuteResponse: &pb.ToolExecuteResponse{
						Result: result,
						Error:  errStr,
					},
				},
			}); err != nil {
				return nil, fmt.Errorf("grpc: send tool_execute response: %w", err)
			}

		case *pb.NodeMessage_StreamEmitRequest:
			callbacks.StreamEmit(m.StreamEmitRequest.Data)

		case *pb.NodeMessage_SandboxExecRequest:
			output, sbErr := callbacks.SandboxExec(ctx, m.SandboxExecRequest.Command)
			errStr := ""
			if sbErr != nil {
				errStr = sbErr.Error()
			}
			if err := stream.Send(&pb.NodeMessage{
				Msg: &pb.NodeMessage_SandboxExecResponse{
					SandboxExecResponse: &pb.SandboxExecResponse{
						Output: output,
						Error:  errStr,
					},
				},
			}); err != nil {
				return nil, fmt.Errorf("grpc: send sandbox_exec response: %w", err)
			}

		case *pb.NodeMessage_SignalRequest:
			var sigPayload any
			if err := json.Unmarshal(m.SignalRequest.Payload, &sigPayload); err != nil {
				sigPayload = m.SignalRequest.Payload
			}
			sigErr := callbacks.Signal(ctx, m.SignalRequest.SignalType, sigPayload)
			errStr := ""
			if sigErr != nil {
				errStr = sigErr.Error()
			}
			if err := stream.Send(&pb.NodeMessage{
				Msg: &pb.NodeMessage_SignalResponse{
					SignalResponse: &pb.SignalResponse{
						Error: errStr,
					},
				},
			}); err != nil {
				return nil, fmt.Errorf("grpc: send signal response: %w", err)
			}
		}
	}
}
