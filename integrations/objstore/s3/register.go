package s3

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/integrations/objstore"
)

// ResourceKind is the deployment resource kind implemented by this
// package.
const ResourceKind = "workspace.Workspace"

// BackendName is the workspace impl name for S3-backed object-store
// workspaces.
const BackendName = "objstore.s3"

type settings struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
}

type factory struct {
	client Client
}

// NewFactory returns a deployment resource factory for an S3-backed
// object-store workspace. The S3 client is a Go value the document
// cannot name; settings carry the bucket and optional key prefix.
func NewFactory(client Client) resource.Factory {
	return factory{client: client}
}

// Spec implements resource.Factory.
func (factory) Spec() resource.Spec {
	return resource.Spec{Kind: ResourceKind, Impl: BackendName}
}

// New implements resource.Factory.
func (f factory) New(_ context.Context, in resource.Input) (any, error) {
	s, err := resource.DecodeTyped[settings](in.Settings, resource.ExpandEnv())
	if err != nil {
		return nil, errdefs.Validationf(
			"objstore s3: decode settings: %v", err)
	}
	if strings.TrimSpace(s.Bucket) == "" {
		return nil, errdefs.Validationf(
			"objstore s3: settings.bucket is required")
	}
	if f.client == nil {
		return nil, errdefs.Validationf(
			"objstore s3: client is required")
	}
	store := New(f.client, s.Bucket)
	return objstore.NewWorkspace(store, objstore.WithPrefix(s.Prefix)), nil
}

// Register adds the S3-backed workspace factory to r.
func Register(r *resource.Registry, client Client) error {
	return r.Register(NewFactory(client))
}
