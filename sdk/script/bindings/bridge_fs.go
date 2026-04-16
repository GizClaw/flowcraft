package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// NewFSBridge exposes workspace file ops as global "fs".
func NewFSBridge(ws workspace.Workspace) BindingFunc {
	return func(ctx context.Context) (string, any) {
		return "fs", map[string]any{
			"read": func(path string) (string, error) {
				if ws == nil {
					return "", nil
				}
				data, err := ws.Read(ctx, path)
				if err != nil {
					return "", err
				}
				return string(data), nil
			},
			"write": func(path, content string) error {
				if ws == nil {
					return nil
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
					return nil
				}
				return ws.Delete(ctx, path)
			},
		}
	}
}
