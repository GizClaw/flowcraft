package sessionkit

// GroupCapsAvailable reports whether the shared process-group watcher can
// enforce MemoryCap/CPUCap in this process: unix, with a working ps(1). Backends
// that delegate resource caps to that watcher must gate their Enforcement on it
// instead of hardcoding true, otherwise they advertise caps that silently never
// fire where ps cannot run.
//
// The probe result is cached for the process lifetime.
func GroupCapsAvailable() bool { return groupCapsAvailable() }
