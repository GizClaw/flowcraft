package policy

import (
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Escalation is what an operator declared above an agent's ceiling: the most a
// session may be widened to, for one command, after somebody agrees.
//
// It is a second layer rather than a second ceiling. A protocol adapter narrows
// a session ([Grant.Narrow]), and a narrowing that a refused command undoes —
// "ask the operator and get the whole ceiling back" — is decorative: the caller
// who asked for less would have been answered with more. So the widening stops
// at what the operator wrote down instead of at the ceiling, and the runtime
// question is only ever "this one command, this one time".
//
// Only three dimensions widen: files.write, net and exec.env. Models, tool
// visibility, delegation, limits, resources and the sandbox are not here,
// because widening one of those changes what the session *is* rather than where
// a single command runs.
type Escalation struct {
	// Files is the write set an escalation may reach. Only Write is read: an
	// escalation cannot widen what a session may read, and Read/ReadOnly are
	// refused at load.
	//
	// Empty Write means "no file widening" — the opposite of the same field on a
	// Grant, where empty means "anywhere". The difference is what the value is
	// for: a Grant states a permission, a declaration states the additions to
	// one, and an addition of nothing has to be spellable.
	Files Files `json:"files,omitempty" yaml:"files,omitempty"`
	// Net is the egress posture an escalation may reach. Absent means the
	// declaration does not widen the network at all.
	//
	// It is a pointer because [NetDefault] already means "no posture imposed", so
	// a value could not tell "not declared" from "unconstrained" — and reading
	// the second as the first is how a declaration silently grants the whole
	// network.
	Net *NetPolicy `json:"net,omitempty" yaml:"net,omitempty"`
	// Exec is the execution policy an escalation may reach. Only Env.Allow is
	// read: a one-off timeout or shell is not a widening, and an injected value
	// hands a command a secret rather than widening a boundary.
	Exec *ExecPolicy `json:"exec,omitempty" yaml:"exec,omitempty"`
	// TTL bounds the lifetime of the temporary cell a widened command runs in. It
	// is required: a widening with no end is a new ceiling.
	TTL Duration `json:"ttl,omitempty" yaml:"ttl,omitempty"`
	// MaxPerSession caps how many times one session may be widened. It is
	// required and positive: a declaration nobody may use is a mistake, and an
	// uncapped one lets a model turn the confine into a prompt it can summon.
	MaxPerSession int `json:"max_per_session,omitempty" yaml:"max_per_session,omitempty"`
}

// Validate reports whether the declaration is a widening of ceiling at all.
//
// It is the load-time gate and it checks the two rules a document can violate on
// its own: an escalation may only carry the widenable dimensions, and it may
// never be narrower than the grant it sits on — a declaration that shrinks
// something would make "widen" a lie, and the runtime would have to guess which
// half of it to honour.
//
// The third invariant — a widening request between the bind and this declaration
// — is [Grant.Widen]'s, because it needs both values.
func (e *Escalation) Validate(ceiling Grant) error {
	if e == nil {
		return nil
	}
	if e.MaxPerSession <= 0 {
		return errdefs.Validationf("policy.escalate: max_per_session must be positive")
	}
	if e.TTL.Std() <= 0 {
		return errdefs.Validationf("policy.escalate: ttl must be a positive duration")
	}
	write, err := normalizeRoots("escalate.files.write", e.Files.Write)
	if err != nil {
		return err
	}
	switch {
	case len(e.Files.Read) > 0:
		return errdefs.Validationf(
			"policy.escalate: files.read is not escalatable; an escalation widens writes only")
	case e.Files.ReadOnly:
		return errdefs.Validationf(
			"policy.escalate: files.readonly is not escalatable; an escalation widens writes only")
	case ceiling.Files.ReadOnly && len(write) > 0:
		return errdefs.Validationf(
			"policy.escalate: files.write cannot widen a read-only grant")
	case len(ceiling.Files.Write) == 0 && len(write) > 0:
		// A grant with no write set may write anywhere, so there is nothing to widen
		// and a declaration that lists roots would shrink it instead.
		return errdefs.Validationf(
			"policy.escalate: files.write is declared but the grant's write set is already unrestricted")
	case !writesContain(write, ceiling.Files.Write):
		return errdefs.Validationf(
			"policy.escalate: files.write %v does not cover the grant's own write set %v",
			write, ceiling.Files.Write)
	}
	if len(write) > 0 && len(ceiling.Files.Read) == 0 {
		// A write root has to sit inside a readable one, or the widened grant is one
		// this package's own validator refuses: [Files] will not accept a write set
		// with no read set beside it.
		return errdefs.Validationf(
			"policy.escalate: files.write needs the grant to declare files.read")
	}
	for _, root := range write {
		if !coveredBy(ceiling.Files.Read, root) {
			return errdefs.Validationf(
				"policy.escalate: files.write %q is outside the grant's read set %v",
				root, ceiling.Files.Read)
		}
	}
	if e.Net != nil {
		if _, err := e.Net.Normalized(); err != nil {
			return errdefs.Validationf("policy.escalate.net: %v", err)
		}
		if !netContains(*e.Net, ceiling.Net) {
			return errdefs.Validationf(
				"policy.escalate: net does not cover the grant's own posture; an escalation only widens")
		}
	}
	if e.Exec != nil {
		if e.Exec.Timeout.Std() != 0 || e.Exec.Shell != "" {
			return errdefs.Validationf(
				"policy.escalate: exec may only carry env; a timeout or a shell is not a widening")
		}
		if len(e.Exec.Env.Inject) > 0 {
			return errdefs.Validationf(
				"policy.escalate: exec.env.inject is not escalatable; injecting hands out a secret rather than widening a boundary")
		}
		if e.Exec.Env.Allow == nil {
			// An exec block that names nothing would widen the environment to
			// "inherit the host's", which is the one reading nobody wrote down.
			return errdefs.Validationf(
				"policy.escalate: exec.env.allow must name the variables to widen to (null means the whole host environment)")
		}
		if err := e.Exec.Validate(); err != nil {
			return errdefs.Validationf("policy.escalate.exec: %v", err)
		}
		if !envContains(e.Exec.Env, ceiling.Exec.Env) {
			return errdefs.Validationf(
				"policy.escalate: exec.env does not cover the grant's own environment; an escalation only widens")
		}
	}
	return nil
}

// Clone returns an independent copy, so a validated [Grant] can hand one out
// without the caller's document reaching back into it.
func (e *Escalation) Clone() *Escalation {
	if e == nil {
		return nil
	}
	out := *e
	out.Files = Files{Read: slices.Clone(e.Files.Read), Write: slices.Clone(e.Files.Write), ReadOnly: e.Files.ReadOnly}
	if e.Net != nil {
		net := *e.Net
		net.AllowHosts = slices.Clone(e.Net.AllowHosts)
		net.Rules = slices.Clone(e.Net.Rules)
		net.UnixSockets = slices.Clone(e.Net.UnixSockets)
		if e.Net.MITM != nil {
			mitm := *e.Net.MITM
			mitm.Hosts = slices.Clone(e.Net.MITM.Hosts)
			mitm.ExcludeHosts = slices.Clone(e.Net.MITM.ExcludeHosts)
			net.MITM = &mitm
		}
		out.Net = &net
	}
	if e.Exec != nil {
		exec := *e.Exec
		exec.Env.Allow = slices.Clone(e.Exec.Env.Allow)
		if e.Exec.Env.Inject != nil {
			exec.Env.Inject = make(map[string]string, len(e.Exec.Env.Inject))
			for name, value := range e.Exec.Env.Inject {
				exec.Env.Inject[name] = value
			}
		}
		out.Exec = &exec
	}
	return &out
}

// Widest returns the widest grant this declaration allows for a session bound by
// bind: what an operator is really being asked about.
//
// It is the request's proposal, not a decision. An escalator may answer with
// anything between bind and this value — an operator who is willing to grant one
// extra directory should not have to grant every one the declaration lists —
// and [Grant.Widen] is what refuses the rest.
func (e *Escalation) Widest(bind Grant) Grant {
	// The declaration is cloned first: what this returns travels to an operator's prompt and
	// back, and the value the deployment validated must not be reachable through it.
	declaration := e.Clone()
	if declaration == nil {
		return bind
	}
	out := bind
	out.Escalate = nil
	if len(declaration.Files.Write) > 0 {
		out.Files.Write = slices.Clone(declaration.Files.Write)
	}
	if declaration.Net != nil {
		out.Net = *declaration.Net
	}
	if declaration.Exec != nil {
		out.Exec.Env.Allow = slices.Clone(declaration.Exec.Env.Allow)
	}
	return out
}

// Widen returns the grant a temporary cell runs under, or the denial that
// refuses it.
//
// The receiver is the session's bind and must carry the deployment's
// [Grant.Escalate]; to is what the operator agreed to. Three conditions hold, and
// each of them is a way the feature could become an authority hole:
//
//   - bind ⊆ to: every permit the session had survives, so a widening cannot
//     quietly drop one and call the result approved;
//   - to ⊆ escalate: nothing the declaration did not name is reached, so an
//     approval widens one boundary rather than replacing it;
//   - everything else identical: a change outside files.write, net and exec.env
//     is a different session, not a wider cell for one command.
//
// A violation is a [Denial] with [CheckEscalation], because it is a rule refusing
// a request like any other. Limits are not compared: [Grant.Digest] deliberately
// leaves them out and a temporary cell does not consult them.
func (g Grant) Widen(to Grant) (Grant, error) {
	declaration := g.Escalate
	if declaration == nil {
		return Grant{}, deny(CheckEscalation, "no escalation is declared for this agent")
	}
	base, err := g.Normalized()
	if err != nil {
		return Grant{}, errdefs.Validationf("policy: the grant being widened is invalid: %w", err)
	}
	// The request may echo the declaration back; it is the receiver's, not its.
	to.Escalate = nil
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
	if !writesContain(declaration.Files.Write, widened.Files.Write) {
		return Grant{}, deny(CheckEscalation,
			"the widened grant reaches a write root the escalation does not declare")
	}
	if declaration.Net == nil {
		if !netContains(base.Net, widened.Net) || !netContains(widened.Net, base.Net) {
			return Grant{}, deny(CheckEscalation,
				"the widened grant changes the network posture the escalation does not declare")
		}
	} else if !netContains(*declaration.Net, widened.Net) {
		return Grant{}, deny(CheckEscalation,
			"the widened grant reaches network authority the escalation does not declare")
	}
	if declaration.Exec == nil {
		if !envContains(base.Exec.Env, widened.Exec.Env) || !envContains(widened.Exec.Env, base.Exec.Env) {
			return Grant{}, deny(CheckEscalation,
				"the widened grant changes the environment the escalation does not declare")
		}
	} else if !envContains(declaration.Exec.Env, widened.Exec.Env) {
		return Grant{}, deny(CheckEscalation,
			"the widened grant sees environment the escalation does not declare")
	}
	widened.Escalate = nil
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

// fixedDigest is the grant with the three widenable dimensions removed: two
// grants with the same value differ only where an escalation may reach.
func fixedDigest(g Grant) string {
	g.Files.Write = nil
	g.Net = NetPolicy{}
	g.Exec.Env = EnvPolicy{}
	g.Escalate = nil
	return g.Digest()
}

// writesContain reports whether every root of narrower is writable under wider.
//
// An empty narrower is "anywhere", which only "anywhere" contains: the empty
// write set is the wider permission in a Grant and the narrower statement in a
// declaration, and both directions are the same comparison seen from a side.
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
