package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// NewFSBridge exposes workspace file ops as global "fs".
func NewFSBridge(ws workspace.Workspace) BindingFunc {
	return func(ctx context.Context) (string, any) {
		return "fs", map[string]any{
			"read": func(path string) (string, error) {
				if ws == nil {
					return "", errdefs.NotAvailablef("fs.read: workspace not configured")
				}
				data, err := ws.Read(ctx, path)
				if err != nil {
					return "", err
				}
				return string(data), nil
			},
			"write": func(path, content string) error {
				if ws == nil {
					return errdefs.NotAvailablef("fs.write: workspace not configured")
				}
				return ws.Write(ctx, path, []byte(content))
			},
			"exists": func(path string) bool {
				if ws == nil {
					return false
				}
				ok, _ := ws.Exists(ctx, path)
				return ok
			},
			"delete": func(path string) error {
				if ws == nil {
					return errdefs.NotAvailablef("fs.delete: workspace not configured")
				}
				return ws.Delete(ctx, path)
			},
		}
	}
}
