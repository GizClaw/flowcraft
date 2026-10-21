package plugin

import "github.com/GizClaw/flowcraft/core/errdefs"

// checkRunGrants refuses a manifest that asks for a grant it has no
// process to use. It lives outside manifest.go because that file is a
// declaration table — the place permissions are named as data — and the
// source audit reads a grant check in it as a gate hiding in the table.
func (m Manifest) checkRunGrants() error {
	if m.Run != nil || !m.HasPermission("tools:provide") {
		return nil
	}
	return errdefs.Validationf(
		"plugin %s: tools:provide requires a run section: "+
			"a plugin contributes tools from its process", m.ID)
}
