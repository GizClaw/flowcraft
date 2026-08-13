//go:build darwin

package seatbelt

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

// buildProfile translates the enforceable part of ExecOptions into an
// SBPL profile. Reads and process execution remain allowed so local
// agents can reach the host toolchain; writes are denied globally and
// re-allowed only under explicitly writable paths.
//
// For NetAllowList / NetProxy the profile allows exactly one outbound
// hole: the loopback port of the host-side enforcement proxy (passed
// as proxyPort). The blanket network deny would also block the Mach /
// AF_SYSTEM sockets macOS needs for TLS and network configuration, so
// those platform sockets are explicitly re-allowed first (same
// exemption set Codex ships in seatbelt_network_policy.sbpl).
func buildProfile(writable []string, net corenet.NetPolicy, proxyPort int) (string, error) {
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
	case corenet.NetDefault:
		// No network rule: host posture, on purpose (the #10390 guard).
	case corenet.NetDenyAll:
		b.WriteString("(deny network*)\n")
	case corenet.NetAllowList, corenet.NetProxy:
		if proxyPort <= 0 {
			return "", errdefs.Internalf(
				"seatbelt: net mode %d requires a positive proxy port, got %d",
				int(net.Mode), proxyPort)
		}
		writeRestrictedNetwork(&b, proxyPort)
	default:
		return "", errdefs.NotAvailablef("seatbelt: unknown net mode %d", int(net.Mode))
	}
	return b.String(), nil
}

// writeRestrictedNetwork emits the allow_list / proxy network section:
// platform sockets macOS needs for TLS and network config, then a
// blanket deny plus a single loopback hole to the enforcement proxy.
func writeRestrictedNetwork(b *strings.Builder, proxyPort int) {
	b.WriteString("; platform sockets macOS needs for TLS + network config;\n")
	b.WriteString("; without these the blanket network deny also blocks\n")
	b.WriteString("; SecurityServer / configd and HTTPS fails at the TLS handshake.\n")
	b.WriteString("(allow system-socket\n")
	b.WriteString("  (require-all\n")
	b.WriteString("    (socket-domain AF_SYSTEM)\n")
	b.WriteString("    (socket-protocol 2)\n")
	b.WriteString("  )\n")
	b.WriteString(")\n")
	b.WriteString("(allow mach-lookup\n")
	for _, name := range []string{
		"com.apple.bsd.dirhelper",
		"com.apple.system.opendirectoryd.membership",
		"com.apple.SecurityServer",
		"com.apple.networkd",
		"com.apple.ocspd",
		"com.apple.trustd.agent",
		"com.apple.SystemConfiguration.DNSConfiguration",
		"com.apple.SystemConfiguration.configd",
	} {
		fmt.Fprintf(b, "  (global-name %s)\n", sbplString(name))
	}
	b.WriteString(")\n")
	b.WriteString("(allow sysctl-read\n")
	b.WriteString("  (sysctl-name-regex #\"^net.routetable\")\n")
	b.WriteString(")\n")
	b.WriteString("(deny network*)\n")
	fmt.Fprintf(b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", proxyPort)
}

// sbplString uses Go's quoted-string escaping, which is compatible with
// SBPL for quotes, backslashes, and control characters.
func sbplString(s string) string {
	return strconv.Quote(s)
}
