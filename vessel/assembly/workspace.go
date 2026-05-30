package assembly

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/vessel"
)

type workspaceBundle struct {
	workspace WorkspaceHandle
	session   vessel.SessionStore
	closers   []func(context.Context) error
}

func buildWorkspace(ctx context.Context, spec WorkspaceSpec, defaults Defaults, catalog *Catalog) (workspaceBundle, error) {
	backend, err := resolveWorkspaceBackend(spec.Backend, defaults, catalog)
	if err != nil {
		return workspaceBundle{}, err
	}
	res, err := backend.BuildWorkspace(ctx, spec)
	if err != nil {
		return workspaceBundle{}, err
	}
	return workspaceBundle{workspace: res.Workspace, session: res.SessionStore, closers: res.Closers}, nil
}

type memoryWorkspaceBackend struct{}

func MemoryWorkspaceBackend() WorkspaceBackend { return memoryWorkspaceBackend{} }

func (memoryWorkspaceBackend) ValidateWorkspace(spec WorkspaceSpec) error {
	if spec.Root != "" {
		return errdefs.Validationf("vessel assembly: workspace.root requires backend %q", WorkspaceBackendFilesystem)
	}
	return nil
}

func (memoryWorkspaceBackend) BuildWorkspace(context.Context, WorkspaceSpec) (WorkspaceResource, error) {
	return WorkspaceResource{
		Workspace:    sdkworkspace.NewMemWorkspace(),
		SessionStore: vessel.NewMemorySessionStore(),
	}, nil
}

type filesystemWorkspaceBackend struct{}

func FilesystemWorkspaceBackend() WorkspaceBackend { return filesystemWorkspaceBackend{} }

func (filesystemWorkspaceBackend) ValidateWorkspace(spec WorkspaceSpec) error {
	if strings.TrimSpace(spec.Root) == "" {
		return errdefs.Validationf("vessel assembly: workspace.root is required for filesystem backend")
	}
	return nil
}

func (filesystemWorkspaceBackend) BuildWorkspace(_ context.Context, spec WorkspaceSpec) (WorkspaceResource, error) {
	ws, err := sdkworkspace.NewLocalWorkspace(spec.Root)
	if err != nil {
		return WorkspaceResource{}, fmt.Errorf("vessel assembly: open workspace: %w", err)
	}
	sessionRoot := filepath.Join(spec.Root, "sessions")
	sessions, err := vessel.NewFilesystemSessionStore(sessionRoot)
	if err != nil {
		return WorkspaceResource{}, err
	}
	return WorkspaceResource{Workspace: ws, SessionStore: sessions}, nil
}
