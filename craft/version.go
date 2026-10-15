package craft

import (
	"runtime/debug"
	"strings"

	"github.com/GizClaw/flowcraft/craft/internal/version"
)

// Version is the craft module version, compared against a definition's
// min_host_version floor.
//
// A binary that linked a released module resolves it from its own build
// info (see [moduleVersion]): the version a consumer pinned is exactly
// what a floor asks about, and the module carries no other way to know
// it — a library is versioned by the tag it was published under, not by
// anything in its own source. A development build resolves to
// [devVersion], where the floor is checked for spelling and skipped:
// a workspace build, a module replaced by a local path, an untagged main
// module, a binary whose build info is missing or unreadable.
//
// A build that stamps the version itself — release tooling linking with
// -ldflags "-X github.com/GizClaw/flowcraft/craft.Version=x.y.z", in the
// dotted numeric spelling the validator accepts — overrides the build
// info, because that is a release saying what it is.
var Version = devVersion

// devVersion is the version nothing resolved: development, floors
// skipped (see [Version]).
const devVersion = "0.0.0"

// modulePath is this module's path in the build info of a binary that
// linked it.
const modulePath = "github.com/GizClaw/flowcraft/craft"

func init() {
	if Version != devVersion {
		// The build stamped one, which is the release's own word.
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if resolved := moduleVersion(info); resolved != "" {
		Version = resolved
	}
}

// moduleVersion resolves this module's version from the build info of
// the binary that linked it, or "" when it does not describe a released
// module: no dependency on this path at all — the module is the main one
// being built, or the binary was built without module metadata — a
// dependency version go does not spell as a version ("(devel)"), or a
// dependency that was replaced, because the code a replacement supplies
// is not the code the version it replaced names.
func moduleVersion(info *debug.BuildInfo) string {
	if info == nil {
		return ""
	}
	for _, dep := range info.Deps {
		if dep == nil || dep.Path != modulePath || dep.Replace != nil {
			continue
		}
		return normalizeVersion(dep.Version)
	}
	return ""
}

// normalizeVersion turns the v-prefixed version go records for a module
// into the dotted numeric spelling this module validates, and reports ""
// for anything else: a development build is a development build, and a
// floor enforced against a version nobody meant is worse than a floor
// skipped.
func normalizeVersion(raw string) string {
	text := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if !version.Valid(text) {
		return ""
	}
	return text
}
