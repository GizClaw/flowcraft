package sessionkit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// classifyStartError maps process-start failures onto errdefs
// categories: a missing binary is NotFound, a permission refusal is
// Forbidden, anything else is Internal. Callers (e.g. script bridges)
// rely on these categories instead of string-matching os/exec errors.
func classifyStartError(cmd string, err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errdefs.NotFound(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	case errors.Is(err, os.ErrPermission):
		return errdefs.Forbidden(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	default:
		return errdefs.Internal(fmt.Errorf("sandbox: exec %s: %w", cmd, err))
	}
}
