//go:build !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package yaml

import (
	"context"

	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

// Notify has no filesystem watch backend on this platform; Reloader.Watch
// falls back to interval polling.
func (s *Store) Notify(context.Context) (<-chan struct{}, error) {
	return nil, config.ErrNotifyUnsupported
}
