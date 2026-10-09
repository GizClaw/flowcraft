package workspace

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Capped reads one workspace file under a byte cap, preferring the
// backend's bounded read ([LimitedReader]) so an oversized file never
// lands in memory whole; a backend without it falls back to a full read
// that is checked afterwards.
//
// Wrapping Read with io.LimitReader is deliberately not the shape here:
// a wrapper can only trim after the fact, and by then the bytes are
// already in memory. The interface lets the backend refuse at the read
// layer, which is the whole point; the fallback exists for backends
// that cannot, and it is a fallback rather than the rule.
func Capped(ctx context.Context, ws Workspace, path string, max int64) ([]byte, error) {
	if max <= 0 {
		return nil, errdefs.Validationf(
			"workspace: read cap must be positive, got %d", max)
	}
	if lr, ok := ws.(LimitedReader); ok {
		data, err := lr.ReadLimited(ctx, path, max)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > max {
			// Defensive: a backend that answers the bounded-read
			// interface but ignores the cap must not reach a decoder
			// either.
			return nil, errdefs.Validationf(
				"workspace: %s is %d bytes, over the %d-byte read cap",
				path, len(data), max)
		}
		return data, nil
	}
	data, err := ws.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errdefs.Validationf(
			"workspace: %s is %d bytes, over the %d-byte read cap",
			path, len(data), max)
	}
	return data, nil
}
