package a2a

import (
	"github.com/GizClaw/flowcraft/core/agent"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// artifactOf turns a remote artifact into a reference the core can carry. A
// peer's file is a URL and our Artifact has a single locator field, so the URL
// is what a consumer sees; a data part is read back through the blob field the
// server writes.
func artifactOf(remote *a2a.Artifact) agent.Artifact {
	artifact := agent.Artifact{
		ID:   string(remote.ID),
		Name: remote.Name,
	}
	for _, part := range remote.Parts {
		switch content := part.Content.(type) {
		case a2a.URL:
			artifact.Blob = string(content)
			artifact.MediaType = part.MediaType
			return artifact
		case a2a.Data:
			fields, ok := content.Value.(map[string]any)
			if !ok {
				continue
			}
			if blob, ok := fields["blob"].(string); ok && blob != "" {
				artifact.Blob = blob
			}
			if mediaType, ok := fields["media_type"].(string); ok {
				artifact.MediaType = mediaType
			}
		}
	}
	return artifact
}
