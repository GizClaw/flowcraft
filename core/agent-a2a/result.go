package a2a

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/wire"
	"github.com/GizClaw/flowcraft/core/message"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
)

var _ agent.ResultReader = (*Runner)(nil)

// Result asks the peer for the task's history and artifacts. Content is the
// final assistant message; the peer's text artifacts are a fallback because a
// proxied answer is carried there, not in a local history.
func (r *Runner) Result(ctx context.Context) (agent.Result, error) {
	r.mu.Lock()
	id := r.lastTaskID
	if id == "" {
		id = r.taskID
	}
	r.mu.Unlock()
	if id == "" {
		return agent.Result{}, agent.ErrNoCheckpoint
	}
	task, err := r.client.GetTask(ctx, &sdk.GetTaskRequest{ID: id})
	if err != nil {
		return agent.Result{}, err
	}
	return resultOf(task), nil
}

func resultOf(task *sdk.Task) agent.Result {
	if task == nil {
		return agent.Result{}
	}
	result := agent.Result{State: wire.FromState(task.Status.State)}

	for i := len(task.History) - 1; i >= 0; i-- {
		msg, err := wire.FromMessage(task.History[i])
		if err != nil {
			continue
		}
		if msg.Role == message.RoleAssistant && msg.Content.Text() != "" {
			result.Content = msg.Content
			break
		}
	}

	var text strings.Builder
	for _, artifact := range task.Artifacts {
		if artifact == nil {
			continue
		}
		for _, part := range artifact.Parts {
			if part == nil {
				continue
			}
			if chunk := part.Text(); chunk != "" {
				text.WriteString(chunk)
				continue
			}
			blob, mediaType := artifactRef(part)
			if blob == "" {
				continue
			}
			name := artifact.Name
			if name == "" {
				name = artifact.Description
			}
			result.Artifacts = append(result.Artifacts, agent.Artifact{
				ID:        string(artifact.ID),
				Name:      name,
				MediaType: mediaType,
				Blob:      blob,
			})
		}
	}
	if result.Content.Text() == "" && text.Len() > 0 {
		result.Content = message.NewTextContent(text.String())
	}

	if result.State == agent.StateInputRequired {
		if question := wire.Text(task.Status.Message); question != "" {
			content := message.NewTextContent(question)
			result.Question = &content
		}
	}
	return result
}

// artifactRef reads the locator a remote artifact carries. A URL is the
// reference; the data shape is what the server's fallback artifact uses.
func artifactRef(part *sdk.Part) (blob, mediaType string) {
	if url := part.URL(); url != "" {
		return string(url), part.MediaType
	}
	switch data := part.Data().(type) {
	case map[string]string:
		return data["blob"], firstNonEmpty(data["media_type"], part.MediaType)
	case map[string]any:
		blob, _ := data["blob"].(string)
		media, _ := data["media_type"].(string)
		return blob, firstNonEmpty(media, part.MediaType)
	}
	return "", ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
