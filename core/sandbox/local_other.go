//go:build !unix

package sandbox

import "os/exec"

// applyProcAttrs is a no-op off unix. The default CommandContext
// behaviour (kill the direct child on cancel) remains in effect.
func applyProcAttrs(*exec.Cmd) {}
