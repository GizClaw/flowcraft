// Package pathtest builds filesystem paths a test can use on any platform.
//
// It exists because the platform disagrees about what "absolute" means: "/work"
// is absolute on unix and is not on Windows, where a path needs a volume. The
// permission and sandbox layers judge a path with filepath.IsAbs, so a fixture
// that hard-codes the unix spelling fails on Windows for a reason that has
// nothing to do with what it is testing.
package pathtest

import goruntime "runtime"

// Roots returns the synthetic absolute root for this platform: "/" on unix and
// "C:/" on Windows. Nothing is created; the value only has to satisfy
// filepath.IsAbs.
func Roots() string {
	if goruntime.GOOS == "windows" {
		return "C:/"
	}
	return "/"
}

// Abs returns name under the synthetic root, joined so a fixture can write the
// same expression on both platforms: Abs("work") is "/work" or "C:/work".
//
// Windows separators are spelled with forward slashes on purpose: Windows accepts
// them, and a backslash inside a double-quoted YAML string is an escape ("\f" is
// a form feed), which would turn a fixture path into a different string entirely.
func Abs(name string) string {
	return Roots() + name
}
