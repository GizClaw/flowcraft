package delegation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

const (
	// StatusToolName is the canonical delegation status lookup tool name.
	StatusToolName = "delegation_status"
	// NoteMetadataKey preserves delegate.note in the backend-neutral metadata.
	NoteMetadataKey = "delegation.note"
)

type delegateTool struct {
	directory sdkdelegation.Directory
}

type statusTool struct{}

type delegateArgs struct {
	Mode     sdkdelegation.Mode `json:"mode"`
	Target   string             `json:"target"`
	Input    string             `json:"input"`
	Metadata map[string]string  `json:"metadata,omitempty"`
	Note     *string            `json:"note,omitempty"`
}

type statusArgs struct {
	DelegationID string `json:"delegation_id"`
}

type response struct {
	DelegationID string               `json:"delegation_id"`
	Status       sdkdelegation.Status `json:"status"`
	Output       string               `json:"output,omitempty"`
	Error        string               `json:"error,omitempty"`
	Metadata     map[string]string    `json:"metadata,omitempty"`
}

// New constructs the delegate and delegation_status tools.
func New(directory sdkdelegation.Directory) []tool.Tool {
	return []tool.Tool{NewDelegate(directory), NewStatus()}
}

// NewDelegate constructs a delegate tool whose Definition discovers targets
// from directory on every call.
func NewDelegate(directory sdkdelegation.Directory) tool.Tool {
	return delegateTool{directory: directory}
}

// NewStatus constructs a delegation_status lookup tool.
func NewStatus() tool.Tool {
	return statusTool{}
}

func (t delegateTool) Definition() tool.Definition {
	targets := t.targets()
	description := "Delegate work to another available target. Supports synchronous results, interaction handoff, and asynchronous execution."
	if len(targets) > 0 {
		var listed strings.Builder
		listed.WriteString(" Available targets: ")
		for i, target := range targets {
			if i > 0 {
				listed.WriteString("; ")
			}
			listed.WriteString(target.ID)
			if target.Description != "" {
				listed.WriteString(" (")
				listed.WriteString(target.Description)
				listed.WriteString(")")
			}
		}
		description += listed.String() + "."
	}
	return definition(sdkdelegation.ToolName, description, targets)
}

func (t delegateTool) targets() []sdkdelegation.Target {
	if t.directory == nil {
		return nil
	}
	targets, err := t.directory.List(context.Background())
	if err != nil || len(targets) == 0 {
		return nil
	}
	valid := make([]sdkdelegation.Target, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.Validate() != nil {
			continue
		}
		if _, duplicate := seen[target.ID]; duplicate {
			continue
		}
		seen[target.ID] = struct{}{}
		valid = append(valid, target)
	}
	return valid
}

func (delegateTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args delegateArgs
	if err := decodeStrict(arguments, &args); err != nil {
		return "", errdefs.Validationf("%s: parse arguments: %v", sdkdelegation.ToolName, err)
	}
	metadata := cloneMetadata(args.Metadata)
	if args.Note != nil {
		if metadata == nil {
			metadata = make(map[string]string, 1)
		}
		metadata[NoteMetadataKey] = *args.Note
	}
	request := sdkdelegation.Request{
		Mode:     args.Mode,
		Target:   args.Target,
		Input:    args.Input,
		Metadata: metadata,
	}
	if err := request.Validate(); err != nil {
		return "", err
	}
	service, err := serviceFromContext(ctx)
	if err != nil {
		return "", err
	}
	result, err := service.Delegate(ctx, request)
	if err != nil {
		return "", err
	}
	if err := result.Validate(); err != nil {
		return "", errdefs.Internal(fmt.Errorf("%s: service returned invalid response: %w", sdkdelegation.ToolName, err))
	}
	switch args.Mode {
	case sdkdelegation.ModeSync:
		if !result.Status.Terminal() {
			return "", errdefs.Internalf("%s: sync response is not terminal: %q", sdkdelegation.ToolName, result.Status)
		}
	case sdkdelegation.ModeHandoff, sdkdelegation.ModeAsync:
		if result.Status != sdkdelegation.StatusAccepted {
			return "", errdefs.Internalf("%s: %s response is not accepted: %q", sdkdelegation.ToolName, args.Mode, result.Status)
		}
	}
	return encodeResponse(result)
}

func (statusTool) Definition() tool.Definition {
	return tool.DefineSchema(
		StatusToolName,
		"Get the latest backend-neutral status of an asynchronous delegation.",
		tool.Property("delegation_id", "string", "The delegation identifier returned by delegate."),
	).Required("delegation_id").DisallowAdditionalProperties().Build()
}

func (statusTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args statusArgs
	if err := decodeStrict(arguments, &args); err != nil {
		return "", errdefs.Validationf("%s: parse arguments: %v", StatusToolName, err)
	}
	if strings.TrimSpace(args.DelegationID) == "" {
		return "", errdefs.Validationf("%s: delegation_id must be non-empty", StatusToolName)
	}
	service, err := serviceFromContext(ctx)
	if err != nil {
		return "", err
	}
	result, err := service.Get(ctx, args.DelegationID)
	if err != nil {
		return "", err
	}
	if err := result.Validate(); err != nil {
		return "", errdefs.Internal(fmt.Errorf("%s: service returned invalid response: %w", StatusToolName, err))
	}
	return encodeResponse(result)
}

func serviceFromContext(ctx context.Context) (sdkdelegation.Service, error) {
	host, ok := agent.HostFromContext(ctx)
	if !ok {
		return nil, errdefs.NotAvailablef("delegation tools: no agent.Host on context")
	}
	service, ok := sdkdelegation.ServiceFromHost(host)
	if !ok {
		return nil, errdefs.NotAvailablef("delegation tools: host has no delegation service")
	}
	return service, nil
}

func encodeResponse(result sdkdelegation.Response) (string, error) {
	raw, err := json.Marshal(response{
		DelegationID: result.ID,
		Status:       result.Status,
		Output:       result.Output,
		Error:        result.Error,
		Metadata:     cloneMetadata(result.Metadata),
	})
	if err != nil {
		return "", errdefs.Internal(fmt.Errorf("delegation tools: encode response: %w", err))
	}
	return string(raw), nil
}

func decodeStrict(arguments string, out any) error {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

func definition(name, description string, targets []sdkdelegation.Target) tool.Definition {
	targetProperty := tool.Property("target", "string", "Receiving delegation target.")
	if len(targets) > 0 {
		ids := make([]string, len(targets))
		for i, target := range targets {
			ids[i] = target.ID
		}
		targetProperty = tool.EnumProperty(
			"target", "string", "Receiving delegation target.", ids...)
	}
	return tool.DefineSchema(
		name,
		description,
		tool.EnumProperty(
			"mode", "string", "Delegation lifecycle mode.",
			string(sdkdelegation.ModeSync),
			string(sdkdelegation.ModeHandoff),
			string(sdkdelegation.ModeAsync)),
		targetProperty,
		tool.Property("input", "string", "Task or user intent for the receiving target."),
		tool.StringMapProperty("metadata", "Optional string metadata."),
		tool.Property("note", "string", "Optional context preserved in delegation metadata."),
	).Required("mode", "target", "input").DisallowAdditionalProperties().Build()
}

func cloneMetadata(metadata map[string]string) map[string]string {
	return maps.Clone(metadata)
}

var (
	_ tool.Tool = delegateTool{}
	_ tool.Tool = statusTool{}
)
