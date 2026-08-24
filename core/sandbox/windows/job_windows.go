//go:build windows

package windows

// jobObjectCapsAvailable reports whether the Job Object resource-cap
// watcher (core/sandbox jobCapsWatcher) can enforce MemoryBytes and
// CPUMillicores. Job objects exist on every supported Windows
// release, so this is a static true; spawn-time failures (e.g. a
// parent job forbidding nesting) surface as Start errors instead of a
// silent capability downgrade.
func jobObjectCapsAvailable() bool { return true }
