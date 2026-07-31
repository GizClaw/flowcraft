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
	MemoryCap        bool
	CPUCap           bool
	DiskCap          bool
	FilesystemBounds bool
}

// EnforcementReporter is implemented by Runners that can describe their
// own enforcement surface. All built-in runners and decorators
// implement it.
type EnforcementReporter interface {
	Enforcement() Enforcement
}

// GroupCapsSupported reports whether the shared process-group watcher
// (StartGroupCapsWatcher) can enforce MemoryCap/CPUCap in this process:
// unix, with a working ps(1). Backends that delegate resource caps to
// that watcher — LocalRunner, sdkx/sandbox/seatbelt — must gate the
// MemoryCap/CPUCap fields of their Enforcement on it instead of
// hardcoding true, otherwise they advertise caps that silently never
// fire in a restricted environment where ps cannot be executed.
//
// The probe result is cached for the process lifetime.
func GroupCapsSupported() bool { return groupCapsAvailable() }

// EnforcementOf returns r.Enforcement() when r implements
// EnforcementReporter, or the conservative zero value otherwise. A nil
// Runner also yields the zero value. Mirrors workspace.CapabilitiesOf.
func EnforcementOf(r Runner) Enforcement {
	if rep, ok := r.(EnforcementReporter); ok {
		return rep.Enforcement()
	}
	return Enforcement{}
}
