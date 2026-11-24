package app

import "runtime/debug"

// forgeHarness names the build this binary is.
//
// It is the one manifest field a document cannot supply and the one an incident is most often
// attributed to, so it is read from what the toolchain recorded at link time rather than from a
// constant somebody has to remember to bump: the module version, plus the VCS revision when the
// build had one.
var forgeHarness = harnessIdentity()

// harnessIdentity renders the build identity from the module's build info.
func harnessIdentity() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "forge"
	}
	identity := "forge@" + info.Main.Version
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			revision := setting.Value
			if len(revision) > 12 {
				revision = revision[:12]
			}
			return identity + "+" + revision
		}
	}
	return identity
}
