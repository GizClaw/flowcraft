package llm

import (
	"github.com/GizClaw/flowcraft/sdk/model"
)

// Re-export model types for backward compatibility.

type Role = model.Role

const (
	RoleSystem    = model.RoleSystem
	RoleUser      = model.RoleUser
	RoleAssistant = model.RoleAssistant
	RoleTool      = model.RoleTool
)

type PartType = model.PartType

const (
	PartText       = model.PartText
	PartImage      = model.PartImage
	PartAudio      = model.PartAudio
	PartFile       = model.PartFile
	PartData       = model.PartData
	PartToolCall   = model.PartToolCall
	PartToolResult = model.PartToolResult
)

type (
	MediaRef       = model.MediaRef
	FileRef        = model.FileRef
	DataRef        = model.DataRef
	ToolCall       = model.ToolCall
	ToolResult     = model.ToolResult
	Part           = model.Part
	Message        = model.Message
	Usage          = model.Usage
	TokenUsage     = model.TokenUsage
	StreamChunk    = model.StreamChunk
	ToolDefinition = model.ToolDefinition
)

var (
	NewTextMessage       = model.NewTextMessage
	NewToolCallMessage   = model.NewToolCallMessage
	NewToolResultMessage = model.NewToolResultMessage
	NewImageMessage      = model.NewImageMessage
	MarshalToolArgs      = model.MarshalToolArgs
)
