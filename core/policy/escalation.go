package policy

import (
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Widen returns the grant a temporary cell runs under, or the denial that refuses it.
//
// The receiver is the authority in force for a session — what the caller stated at open — and to
// is what the operator approved. There is no declared ceiling above it: the bound is what the
// process can enforce, and [sandbox.ValidateConfine] answers that when the temporary cell is
// built. Three conditions hold here, and each of them is a way the feature could become an
// authority hole:
//
//   - base ⊆ to: every permit the session had survives, so a widening cannot quietly drop one and
//     call the result approved;
//   - only files.write, net and exec.env differ: a change anywhere else is a different session, not
//     a wider cell for one command;
//   - to validates on its own, so an approval cannot hand the cell a grant this package would
//     refuse.
//
// A violation is a [Denial] with [CheckEscalation], because it is a rule refusing a request like
// any other. The separation is what keeps an approval honest: the operator widens one boundary
// inside what the machine can enforce, and the temporary cell exists only for that command.
func (g Grant) Widen(to Grant) (Grant, error) {
	base, err := g.Normalized()
	if err != nil {
		return Grant{}, errdefs.Validationf("policy: the grant being widened is invalid: %w", err)
	}
	widened, err := to.Normalized()
	if err != nil {
		return Grant{}, errdefs.Validationf("policy: the grant to widen to is invalid: %w", err)
	}
	if fixedDigest(base) != fixedDigest(widened) {
		return Grant{}, deny(CheckEscalation,
			"only files.write, net and exec.env may be widened")
	}
	if !writesContain(widened.Files.Write, base.Files.Write) {
		return Grant{}, deny(CheckEscalation,
			"the widened grant does not keep every root the session may already write")
	}
	if !netContains(widened.Net, base.Net) {
		return Grant{}, deny(CheckEscalation,
			"the widened grant does not keep every destination the session may already reach")
	}
	if !envContains(widened.Exec.Env, base.Exec.Env) {
		return Grant{}, deny(CheckEscalation,
			"the widened grant does not keep every variable the session may already see")
	}
	return widened, nil
}

// Change is one dimension a widening touches, in the form an approval prompt
// renders: the values, never the command line.
type Change struct {
	// Dimension names the grant field: "files.write", "net" or "exec.env".
	Dimension string `json:"dimension" yaml:"dimension"`
	// Before and After are the dimension's permits, one per entry.
	Before []string `json:"before,omitempty" yaml:"before,omitempty"`
	After  []string `json:"after,omitempty" yaml:"after,omitempty"`
}

// Diff lists the dimensions from and to disagree on, in a fixed order, so a
// prompt reads the same way every time.
//
// It takes grants rather than a command on purpose: what an operator is being
// asked to approve is a boundary, and a signature that could also carry argv
// would be one prompt away from putting a secret in a log.
func Diff(from, to Grant) []Change {
	var changes []Change
	if !slices.Equal(from.Files.Write, to.Files.Write) {
		changes = append(changes, Change{
			Dimension: "files.write",
			Before:    slices.Clone(from.Files.Write),
			After:     slices.Clone(to.Files.Write),
		})
	}
	if before, after := netLines(from.Net), netLines(to.Net); !slices.Equal(before, after) {
		changes = append(changes, Change{Dimension: "net", Before: before, After: after})
	}
	if before, after := envLines(from.Exec.Env), envLines(to.Exec.Env); !slices.Equal(before, after) {
		changes = append(changes, Change{Dimension: "exec.env", Before: before, After: after})
	}
	return changes
}

// fixedDigest is the grant with the three widenable dimensions removed: two grants with the same
// value differ only where an approval may reach.
func fixedDigest(g Grant) string {
	g.Files.Write = nil
	g.Net = NetPolicy{}
	g.Exec.Env = EnvPolicy{}
	return g.Digest()
}

// writesContain reports whether every root of narrower is writable under wider.
//
// An empty narrower is "anywhere", which only "anywhere" contains.
func writesContain(wider, narrower []string) bool {
	if len(narrower) == 0 {
		return len(wider) == 0
	}
	for _, root := range narrower {
		if !coveredBy(wider, root) {
			return false
		}
	}
	return true
}

// netContains reports whether wider permits everything narrower permits.
//
// It is deliberately conservative: a posture it cannot prove contained is not
// contained, because the caller uses the answer to hand out authority. Modes go
// deny_all ⊆ allow_list, an unconfigured posture ("leave it to the host") covers
// everything, and two different isolating postures are only comparable when they
// are the same one. Rules are matched in order, so a rule list is contained only
// by itself.
func netContains(wider, narrower NetPolicy) bool {
	if wider.IsZero() {
		return true
	}
	if narrower.IsZero() {
		return false
	}
	if !modeContains(wider.Mode, narrower.Mode) {
		return false
	}
	if wider.Proxy != narrower.Proxy {
		return false
	}
	if !slices.Equal(wider.Rules, narrower.Rules) {
		return false
	}
	for _, host := range narrower.AllowHosts {
		if !hostsCover(wider.AllowHosts, host) {
			return false
		}
	}
	for _, socket := range narrower.UnixSockets {
		if !slices.Contains(wider.UnixSockets, socket) {
			return false
		}
	}
	return mitmContains(wider.MITM, narrower.MITM)
}

// modeContains reports whether a posture of mode wider admits everything one of
// mode narrower does.
func modeContains(wider, narrower NetMode) bool {
	switch {
	case wider == narrower, wider == NetDefault:
		return true
	case wider == NetAllowList && narrower == NetDenyAll:
		return true
	default:
		return false
	}
}

// hostsCover reports whether one allow-list covers a single entry.
//
// An entry without a port matches any port; a hostname entry covers its
// subdomains, which is how the allow-list is documented; an IP literal matches
// itself only, because a suffix rule over numbers would make 10.0.1 cover
// 0.1 by accident.
func hostsCover(wider []string, narrower string) bool {
	narrowHost, narrowPort := splitHost(narrower)
	for _, entry := range wider {
		wideHost, widePort := splitHost(entry)
		if widePort != 0 && widePort != narrowPort {
			continue
		}
		if wideHost == narrowHost {
			return true
		}
		if net.ParseIP(wideHost) != nil || net.ParseIP(narrowHost) != nil {
			continue
		}
		if strings.HasSuffix(narrowHost, "."+wideHost) {
			return true
		}
	}
	return false
}

// splitHost separates an allow-list entry into its host and its port.
func splitHost(entry string) (string, int) {
	host, port, found := strings.Cut(entry, ":")
	if !found {
		return entry, 0
	}
	parsed, err := net.LookupPort("tcp", port)
	if err != nil {
		return entry, 0
	}
	return host, parsed
}

// mitmContains reports whether wider intercepts at least what narrower did.
//
// Interception is not a widening dimension — it inspects traffic rather than
// permitting it — so the only direction that matters is that a widening never
// removes inspection the session had: a disabled posture in wider means wider
// permits everything, which always contains.
func mitmContains(wider, narrower *MITMPolicy) bool {
	if narrower == nil || !narrower.Enabled {
		return true
	}
	if wider == nil || !wider.Enabled {
		return true
	}
	return wider.InspectBodies == narrower.InspectBodies &&
		wider.MaxBodyBytes == narrower.MaxBodyBytes &&
		slices.Equal(wider.Hosts, narrower.Hosts) &&
		slices.Equal(wider.ExcludeHosts, narrower.ExcludeHosts)
}

// envContains reports whether wider lets the child see everything narrower did.
//
// A nil allow-list is "inherit the host environment", which is the widest
// statement there is; an explicit list is contained by a nil one and by a
// superset of itself.
func envContains(wider, narrower EnvPolicy) bool {
	if narrower.Allow == nil {
		return wider.Allow == nil
	}
	if wider.Allow == nil {
		return true
	}
	for _, name := range narrower.Allow {
		if !slices.Contains(wider.Allow, name) {
			return false
		}
	}
	return true
}

// netLines renders a network posture as one line per permit, for a prompt.
//
// A proxy is rendered by its host and never by its URL: the URL may carry
// credentials, and a prompt is a place they would leave the process.
func netLines(p NetPolicy) []string {
	if p.IsZero() {
		return nil
	}
	lines := []string{"mode=" + p.Mode.String()}
	for _, host := range p.AllowHosts {
		lines = append(lines, "allow "+host)
	}
	for _, rule := range p.Rules {
		lines = append(lines, "rule "+rule.String())
	}
	if p.Proxy != "" {
		lines = append(lines, "proxy "+proxyHost(p.Proxy))
	}
	for _, socket := range p.UnixSockets {
		lines = append(lines, "unix "+socket)
	}
	if p.MITM != nil && p.MITM.Enabled {
		lines = append(lines, "mitm")
	}
	return lines
}

// proxyHost is a proxy URL's host, or "configured" when it does not parse: what a
// prompt needs is "there is a proxy", not its credentials.
func proxyHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "configured"
	}
	return parsed.Host
}

// envLines renders an environment policy as one line per visible name, for a
// prompt.
func envLines(p EnvPolicy) []string {
	if p.Allow == nil {
		return []string{"env=all"}
	}
	lines := make([]string, 0, len(p.Allow))
	for _, name := range p.Allow {
		lines = append(lines, "allow "+name)
	}
	return lines
}
