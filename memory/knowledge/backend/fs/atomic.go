package fs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// atomicWrite writes data to dst atomically: it stages the payload in a
// sibling tmp path and then Renames into place. Workspace.Rename is
// required to be POSIX-atomic on local filesystems; object-store
// backends fall back to copy + delete with the same external semantics.
//
// Tmp paths use a random suffix so concurrent writers to the same dst
// do not collide on the staging path.
func atomicWrite(ctx context.Context, ws workspace.Workspace, dst string, data []byte) error {
	if ws == nil {
		return errdefs.Validationf("knowledge/fs: nil workspace")
	}
	tmp := dst + ".tmp." + randomSuffix()
	if err := ws.Write(ctx, tmp, data); err != nil {
		return fmt.Errorf("knowledge/fs: write tmp %q: %w", tmp, err)
	}
	if err := ws.Rename(ctx, tmp, dst); err != nil {
		_ = ws.Delete(ctx, tmp)
		return fmt.Errorf("knowledge/fs: rename %q -> %q: %w", tmp, dst, err)
	}
	return nil
}

// randomSuffix returns 8 hex chars suitable for a tmp filename.
func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
