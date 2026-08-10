// Package seatbelt implements sdk/sandbox.Runner on top of Apple's
// sandbox-exec (Seatbelt / SBPL) — the only built-in confinement
// primitive on macOS. It is the macOS sibling of sdkx/sandbox/bwrap:
// same policy surface, different enforcement kernel.
//
// # Why sdkx
//
// sdk defines interfaces and primitives; sdkx ships concrete adapters
// that integrate with external systems. sandbox-exec is an external
// binary shipped with macOS, wrapped the same way bubblewrap is wrapped on
// Linux. The Runner type implements the generic sandbox.Runner
// interface, so a caller can be retargeted between LocalRunner, this
// backend, and bwrap without changing call sites.
//
// # macOS-only
//
// sandbox-exec exists only on macOS. The Runner type is therefore only
// constructible on darwin; on other platforms [New] returns
// errdefs.NotAvailable so callers can import the package for type
// references without build-tag gymnastics.
//
// # Capability matrix vs. sandbox.LocalRunner
//
// Mapping of sandbox.ExecOptions fields onto the Seatbelt profile:
//
//	WorkDir                     -- chdir; writes confined to the root
//	Stdin                       piped via os/exec
//	Timeout                     Go-side ctx deadline + process-group kill
//	Env.Allow / Env.Inject      filtered in Go (c.Env); Seatbelt has no env concept
//	Net.Mode == NetDefault      no network rules (host posture)
//	Net.Mode == NetDenyAll      (deny network*)
//	Net.Mode == NetAllowList    loopback-only profile + host enforcement proxy (hostname allow-list)
//	Net.Mode == NetProxy        loopback-only profile + host enforcement proxy (upstream)
//	Resources.MemoryBytes       group watcher (sdk/sandbox.GroupCapsWatcher)
//	Resources.CPUMillicores     group watcher, cpu-time = Timeout x millicores/1000
//	Resources.DiskBytes         errdefs.NotAvailable (no quota mechanism)
//	Resources.MaxOutputBytes    truncated in Go (limitedBuffer)
//
// # Blast-radius policy shape
//
// The generated profile reads as "allow everything, deny all writes,
// re-allow the workspace": reads and process execution are unrestricted
// (a local agent must reach the real toolchain), file writes are denied
// machine-wide except the runner root and /dev/null. Dedicated temp
// or cache paths may be added explicitly with WithWritablePaths. This
// is the containment posture the local-sandbox PRD
// calls blast-radius: not total isolation, but an honest boundary
// around the workspace.
//
// # NetAllowList / NetProxy enforcement
//
// Both modes run the child under a profile that denies all network
// except one loopback port, which a host-side enforcement proxy
// (sdkx/internal/httpkit) listens on. The proxy evaluates the
// allow-list (NetAllowList) or forwards to the configured upstream
// (NetProxy); it resolves hostnames on the host, so the child needs no
// resolver route. The child env is forced onto the proxy loopback
// (HTTP(S)_PROXY / ALL_PROXY) and NO_PROXY is stripped, so proxy-aware
// clients use the single enforced egress.
//
// The blanket network deny also covers the Mach / AF_SYSTEM sockets
// macOS needs for TLS and network configuration, so the restricted
// profile explicitly re-allows the same platform services Codex ships
// (SecurityServer, trustd, configd, networkd, ...). See
// writeRestrictedNetwork.
//
// Limitations (v1): only proxy-aware clients (HTTP(S)_PROXY) are
// supported — raw TCP/UDP applications fail closed; there is no UDP
// proxying; AllowHosts matches hostname suffixes or exact IP literals
// with all ports allowed. SBPL network rules do not confine unix-domain
// sockets; the macOS sensitive-UDS surface is small, so this is a
// documented boundary rather than a broad file-read deny.
//
// Note: sandbox-exec is formally deprecated by Apple yet remains
// functional and is the same primitive Chrome and Anthropic's
// sandbox-runtime rely on. A future Virtualization.framework backend
// can supersede this package without changing the Runner seam.
//
// # Interactive sessions
//
// Runner also implements sandbox.ProcessManager: sessions run inside
// the same generated SBPL profile as Exec (including the enforcement
// proxy for NetAllowList / NetProxy, which lives as long as the
// session). Stdio is either a pty (TTY: true) or tagged pipes, with
// the seq-cursor replay contract defined in sdk/sandbox.
//
// # Proxy enhancements
//
// The host-side enforcement proxy supports rule-based allow/deny,
// socks5:// upstreams, MITM (TLS termination + hooks) with
// SSL_CERT_FILE injection, and per-decision audit callbacks. MITM
// terminates both HTTP/1.1 and HTTP/2 clients (ALPN h2 + http/1.1);
// MITMPolicy.Hosts / ExcludeHosts select which CONNECT tunnels are
// terminated, so cert-pinning destinations can be raw-tunnelled
// explicitly. Unix socket allow-lists are not enforceable under SBPL,
// so any non-empty UnixSockets policy is rejected with
// errdefs.NotAvailable.
//
// macOS CA-injection caveat: Go and curl on macOS use the Security
// framework / SecureTransport by default and ignore SSL_CERT_FILE,
// and under SBPL the Security framework is unreachable (the child's
// system pool is empty). Programs must explicitly load the injected
// bundle (Go: tls.Config.RootCAs from SSL_CERT_FILE; curl:
// --cacert "$SSL_CERT_FILE") — the environment variable is the
// carrier, not the automatic trust source. This matches the
// blast-radius posture: only bundle-aware programs get MITM'd
// transparently; the rest fail TLS rather than trusting silently.
package seatbelt
