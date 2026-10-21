package protocol

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The wire version this build speaks. Only the major is a compatibility
// boundary: a peer with a different major is refused outright, while a
// higher minor is accepted because one major may only grow additively.
const (
	Major   = 1
	Minor   = 0
	Version = "1.0"
)

// ParseVersion splits a major.minor wire version. A version that is not
// two decimal numbers is a validation failure: the peer is speaking
// something this build cannot even name.
func ParseVersion(v string) (uint32, uint32, error) {
	text := strings.TrimSpace(v)
	if text == "" {
		return 0, 0, errdefs.Validationf("plugin wire: empty wire version")
	}
	parts := strings.Split(text, ".")
	if len(parts) != 2 {
		return 0, 0, errdefs.Validationf(
			"plugin wire: wire version %q is not major.minor", v)
	}
	major, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, errdefs.Validationf(
			"plugin wire: wire version %q has an invalid major", v)
	}
	minor, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, 0, errdefs.Validationf(
			"plugin wire: wire version %q has an invalid minor", v)
	}
	return uint32(major), uint32(minor), nil
}

// CheckCompatible reports whether a peer's wire version can be served by
// this build. The error is a validation class on purpose: a mismatched
// major is terminal, which is what makes an enable roll back instead of
// waiting out a process that can never become ready.
func CheckCompatible(v string) error {
	major, _, err := ParseVersion(v)
	if err != nil {
		return err
	}
	if major != Major {
		return errdefs.Validationf(
			"plugin wire: version %q is not compatible with %d; "+
				"this host speaks major %d", v, Major, Major)
	}
	return nil
}

// NegotiatedVersion is the version craft answers a Hello with: this
// build's major, and the lower of the two minors.
func NegotiatedVersion(peer string) (string, error) {
	major, minor, err := ParseVersion(peer)
	if err != nil {
		return "", err
	}
	if major != Major {
		return "", errdefs.Validationf(
			"plugin wire: version %q is not compatible with %d", peer, Major)
	}
	if minor > Minor {
		minor = Minor
	}
	return fmt.Sprintf("%d.%d", Major, minor), nil
}
