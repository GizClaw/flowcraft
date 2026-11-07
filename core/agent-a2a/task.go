package a2a

import (
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// Task is the A2A task shape.
type Task struct {
	Kind      string     `json:"kind"`
	ID        string     `json:"id"`
	ContextID string     `json:"contextId,omitempty"`
	Status    Status     `json:"status"`
	History   []Message  `json:"history,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// Status is a task status with an optional message.
type Status struct {
	State   string   `json:"state"`
	Message *Message `json:"message,omitempty"`
}

// Message is one A2A message.
type Message struct {
	Kind      string `json:"kind"`
	MessageID string `json:"messageId,omitempty"`
	Role      string `json:"role"`
	Parts     []Part `json:"parts"`
}

// Part is a text, data or file part. The kinds core does not have a wire form
// for are rejected by ConvertMessage rather than silently dropped.
type Part struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	Data any    `json:"data,omitempty"`
	File *File  `json:"file,omitempty"`
}

// File is the file part payload.
type File struct {
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Artifact is one produced artifact.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []Part `json:"parts"`
}

// ConvertTask renders one internal Task as the A2A shape.
func ConvertTask(task agent.Task) (Task, error) {
	history := make([]Message, 0, len(task.History))
	for _, msg := range task.History {
		converted, err := ConvertMessage(msg)
		if err != nil {
			return Task{}, err
		}
		history = append(history, converted)
	}
	artifacts := make([]Artifact, 0, len(task.Artifacts))
	for _, artifact := range task.Artifacts {
		artifacts = append(artifacts, Artifact{
			ArtifactID: artifact.ID,
			Name:       artifact.Name,
			Parts: []Part{{
				Kind: "file",
				File: &File{URI: artifact.Blob, MimeType: artifact.MediaType, Name: artifact.Name},
			}},
		})
	}
	return Task{
		Kind:      "task",
		ID:        task.ID,
		ContextID: task.ContextID,
		Status:    Status{State: string(task.State)},
		History:   history,
		Artifacts: artifacts,
	}, nil
}

// ConvertMessage renders one core message as an A2A message.
func ConvertMessage(msg message.Message) (Message, error) {
	role := "agent"
	switch msg.Role {
	case message.RoleUser:
		role = "user"
	case message.RoleSystem:
		// A2A has no system role; the system prompt is the agent's own framing
		// and is not part of the transcript a client sees.
		return Message{}, errdefs.Validationf("a2a: system messages have no wire form")
	}
	parts := make([]Part, 0, len(msg.Content.Parts))
	for _, part := range msg.Content.Parts {
		converted, err := ConvertPart(part)
		if err != nil {
			return Message{}, err
		}
		parts = append(parts, converted)
	}
	return Message{Kind: "message", Role: role, Parts: parts}, nil
}

// ConvertPart renders one content part. Media parts have no A2A representation
// in this adapter yet, so they are an error rather than an omission: a client
// that silently loses an image cannot tell the difference from one that was
// never sent.
func ConvertPart(part message.Part) (Part, error) {
	switch typed := part.(type) {
	case message.TextPart:
		return Part{Kind: "text", Text: typed.Text}, nil
	case message.DataPart:
		return Part{Kind: "data", Data: typed.Value}, nil
	case message.FilePart:
		return Part{Kind: "file", File: &File{
			URI: typed.URI, Name: typed.Name, MimeType: typed.MediaType,
		}}, nil
	case message.ReasoningPart:
		// Reasoning is the model's own scratch space, not an answer to the user.
		return Part{}, errdefs.Validationf("a2a: reasoning parts have no wire form")
	default:
		return Part{}, errdefs.Validationf("a2a: part kind %q has no wire form", part.Kind())
	}
}
