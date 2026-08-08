package s3

import (
	"context"
	"strings"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
	"github.com/GizClaw/flowcraft/sdkx/workspace/objstore"
)

// BackendName is the workspace config driver name for S3-backed
// object-store workspaces.
const BackendName = "objstore.s3"

type settings struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
}

// Register adds an object-store-backed workspace driver to a workspace
// config builder. The S3 client is a Go value the document cannot
// name; settings carry the bucket and optional key prefix.
func Register(b *workspaceconfig.Builder, client Client) error {
	return b.RegisterFactory(BackendName, func(
		_ context.Context,
		in sdkconfig.Input,
	) (workspaceconfig.Resource, error) {
		s, err := sdkconfig.DecodeSettings[settings](in.Settings)
		if err != nil {
			return workspaceconfig.Resource{}, errdefs.Validationf(
				"objstore s3: decode settings: %v", err)
		}
		if strings.TrimSpace(s.Bucket) == "" {
			return workspaceconfig.Resource{}, errdefs.Validationf(
				"objstore s3: settings.bucket is required")
		}
		store := New(client, s.Bucket)
		ws := objstore.NewWorkspace(store, objstore.WithPrefix(s.Prefix))
		return workspaceconfig.Resource{Workspace: ws}, nil
	})
}
