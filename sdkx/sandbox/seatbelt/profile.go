package seatbelt

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// buildProfile translates the enforceable part of ExecOptions into an
// SBPL profile. Reads and process execution remain allowed so local
// agents can reach the host toolchain; writes are denied globally and
// re-allowed only under explicitly writable paths.
func buildProfile(writable []string, net sandbox.NetPolicy) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")
	b.WriteString("(deny file-write*)\n")
	for _, path := range writable {
		if path == "" {
			continue
		}
		fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", sbplString(path))
	}
	b.WriteString("(allow file-write* (literal \"/dev/null\"))\n")

	switch net.Mode {
	case sandbox.NetDefault:
	case sandbox.NetDenyAll:
		b.WriteString("(deny network*)\n")
	case sandbox.NetAllowList:
		return "", errdefs.NotAvailablef(
			"seatbelt: net allow-list not supported; hostname-safe enforcement requires a proxy",
		)
	case sandbox.NetProxy:
		return "", errdefs.NotAvailablef(
			"seatbelt: net proxy mode not supported; Seatbelt has no redirect primitive",
		)
	default:
		return "", errdefs.NotAvailablef("seatbelt: unknown net mode %d", int(net.Mode))
	}
	return b.String(), nil
}

// sbplString uses Go's quoted-string escaping, which is compatible with
// SBPL for quotes, backslashes, and control characters.
func sbplString(s string) string {
	return strconv.Quote(s)
}
