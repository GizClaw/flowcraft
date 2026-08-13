package sandbox

// Enforcement reports which policy dimensions a Runner can actually
// enforce on the current platform, so callers and UIs never have to
// guess from trial calls. It mirrors the workspace.Capabilities
// philosophy — conservative false means "not enforced", never
// "unknown" — but is kept a distinct type because composition differs:
// sandbox decorators intersect what the chain can enforce, whereas
// workspace sub-views forward the parent's storage semantics.
//
// Field semantics:
//
//   - EnvAllowList: the runner honours EnvPolicy.Allow (drops host
//     variables not on the list) rather than ignoring the field.
//   - NetModes: the set of NetMode values the backend can enforce at
//     the OS level. NetDefault is never listed — it is the absence of
//     a policy, not an enforceable posture.
//   - Socks5: the backend's host-side proxy can dial a socks5://
//     upstream (authentication included).
//   - MITM: the backend can terminate TLS for configured CONNECT
//     hosts and inject the temporary CA into the child environment.
//   - UnixSocketPolicy: the backend can confine unix socket egress to
//     an explicit allow-list. For bwrap this is namespace-visibility
//     based (masked dirs deny, listed paths are bind-mounted in), so
//     the claim is strongest in the isolated net modes.
//   - MemoryCap: MemoryBytes is enforced (by whatever mechanism the
//     backend has — cgroup, rlimit, or a sampling watcher) rather
//     than rejected with NotAvailable.
//   - CPUCap: CPUMillicores (with Timeout) is likewise enforced.
//   - DiskCap: DiskBytes is enforced. No local backend reports this
//     today.
//   - FilesystemBounds: writes are confined to the runner root at the
//     OS level (Seatbelt profile, namespace mounts). LocalRunner's
//     WorkDir check is call-time validation only — once the child is
//     running it can chdir anywhere — so it does not qualify.
type Enforcement struct {
	EnvAllowList     bool
	NetModes         []NetMode
	Socks5           bool
	MITM             bool
	UnixSocketPolicy bool
	MemoryCap        bool
	CPUCap           bool
	DiskCap          bool
	FilesystemBounds bool
}

// GroupCapsSupported reports whether the shared process-group watcher
// (StartGroupCapsWatcher) can enforce MemoryCap/CPUCap in this process:
// unix, with a working ps(1). Backends that delegate resource caps to
// that watcher — LocalRunner, backends/sandbox/seatbelt — must gate the
// MemoryCap/CPUCap fields of their Enforcement on it instead of
// hardcoding true, otherwise they advertise caps that silently never
// fire in a restricted environment where ps cannot be executed.
//
// The probe result is cached for the process lifetime.
func GroupCapsSupported() bool { return groupCapsAvailable() }
