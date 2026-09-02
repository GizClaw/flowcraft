package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/workspace"
)

const (
	// defaultFSReadMaxBytes bounds one fs.read call. It mirrors the
	// resource.Loader default so a single script cannot pull an
	// arbitrarily large file into process memory ([]byte -> string
	// copies it again).
	defaultFSReadMaxBytes int64 = 1 << 20
	// defaultFSWriteMaxBytes bounds one fs.write call so a script
	// cannot fill the host disk with a single write.
	defaultFSWriteMaxBytes int64 = 8 << 20
)

// FSBridgeOption configures an fs bridge.
type FSBridgeOption func(*fsBridgeConfig)

type fsBridgeConfig struct {
	readMaxBytes  int64
	writeMaxBytes int64
}

// WithMaxReadBytes caps the size of files fs.read may return.
// Non-positive values disable the cap.
func WithMaxReadBytes(n int64) FSBridgeOption {
	return func(c *fsBridgeConfig) { c.readMaxBytes = n }
}

// WithMaxWriteBytes caps the size of content fs.write accepts.
// Non-positive values disable the cap.
func WithMaxWriteBytes(n int64) FSBridgeOption {
	return func(c *fsBridgeConfig) { c.writeMaxBytes = n }
}

// NewFSBridge exposes workspace file ops as global "fs".
func NewFSBridge(ws workspace.Workspace, opts ...FSBridgeOption) BindingFunc {
	cfg := fsBridgeConfig{
		readMaxBytes:  defaultFSReadMaxBytes,
		writeMaxBytes: defaultFSWriteMaxBytes,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return func(ctx context.Context) (string, any) {
		return "fs", map[string]any{
			"read": func(path string) (string, error) {
				if ws == nil {
					return "", errdefs.NotAvailablef("fs.read: workspace not configured")
				}
				if cfg.readMaxBytes > 0 {
					lr, ok := ws.(workspace.LimitedReader)
					if !ok {
						return "", errdefs.NotAvailablef(
							"fs.read: workspace %T does not support bounded reads", ws)
					}
					data, err := lr.ReadLimited(ctx, path, cfg.readMaxBytes)
					if err != nil {
						return "", err
					}
					return string(data), nil
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
				if cfg.writeMaxBytes > 0 && int64(len(content)) > cfg.writeMaxBytes {
					return errdefs.Validationf(
						"fs.write: %s is %d bytes, exceeds max %d bytes",
						path, len(content), cfg.writeMaxBytes)
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
