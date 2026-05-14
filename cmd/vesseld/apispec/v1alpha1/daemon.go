package v1alpha1

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Daemon is the singleton document describing the daemon process
// itself: which sockets to bind, the global concurrency budget,
// shared LLM provider rate limits, logging, and the SIGTERM
// drain timeout. The loader requires exactly one Daemon document
// across the entire --config input set.
//
// Why a kind and not just CLI flags: configuration files travel
// through git review and CI; CLI flags do not. Keeping daemon
// settings in a kind means a config repo fully describes the
// production posture without a separate launch script.
type Daemon struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       DaemonSpec `json:"spec" yaml:"spec"`
}

// DaemonSpec is the configurable surface of the daemon.
type DaemonSpec struct {
	// Control bundles the listener parameters. At least one of
	// control.socket / control.listen must be non-empty; otherwise
	// the daemon would have no admin interface and could only be
	// stopped via SIGTERM.
	Control DaemonControl `json:"control" yaml:"control"`

	// Resources caps the daemon-wide concurrent run count. 0
	// disables the cap (per-vessel limits still apply). The cap
	// is applied as a semaphore wrapping every Captain dispatch
	// invocation, which is why it lives at this layer rather
	// than inside vessel/.
	Resources DaemonResources `json:"resources,omitempty" yaml:"resources,omitempty"`

	// LLMRateLimits applies cross-vessel rate limits keyed by
	// LLMProfile name. An entry with both fields zero is
	// equivalent to "no entry" and is ignored.
	LLMRateLimits []LLMRateLimit `json:"llmRateLimits,omitempty" yaml:"llmRateLimits,omitempty"`

	// Logging controls structured log emission. Implementation is
	// in cmd/vesseld/runtime; the schema is defined here so the
	// validator catches typos early.
	Logging DaemonLogging `json:"logging,omitempty" yaml:"logging,omitempty"`

	// Shutdown collects the lifecycle timeouts. drainTimeout is
	// the only field today; we still wrap it in a struct so
	// adding shutdown-related fields later does not require a
	// schema bump.
	Shutdown DaemonShutdown `json:"shutdown,omitempty" yaml:"shutdown,omitempty"`

	// SessionStore, when non-nil, provisions a per-run
	// workspace.Workspace for every dispatched agent.Run via
	// vessel.WithSessionStore. The same instance is shared
	// across every Captain in the daemon (run IDs are globally
	// unique within a daemon's lifetime, so sharing is
	// race-free). When omitted, tools that depend on a
	// workspace must fall back to their own wiring — there is
	// intentionally no default backend because the choice
	// between in-memory (ephemeral, fast) and filesystem
	// (persistent, restart-survivable) is workload-dependent.
	SessionStore *DaemonSessionStore `json:"sessionStore,omitempty" yaml:"sessionStore,omitempty"`
}

// DaemonSessionStore selects the backend that materialises per-run
// workspaces. v0.2.0 ships two backends; new ones (e.g. Redis,
// object-storage) would slot in here additively without changing
// the field's shape.
type DaemonSessionStore struct {
	// Backend names the implementation. Allowed values:
	//
	//   "memory"     — in-process vessel.MemorySessionStore.
	//                  Workspaces vanish on daemon restart; good
	//                  for ephemeral CI runs and integration tests.
	//   "filesystem" — vessel.FilesystemSessionStore rooted at
	//                  Root. Workspaces persist across restarts so
	//                  a resumed run sees the same files it left.
	//                  Root is required.
	//
	// Required.
	Backend string `json:"backend" yaml:"backend"`

	// Root is the on-disk root for "filesystem". Each run gets
	// "<Root>/<runID>/" as its workspace. Required when
	// Backend == "filesystem"; rejected when Backend == "memory"
	// to keep the schema honest (the "memory" backend simply
	// cannot use Root, so accepting it silently would mask
	// typos).
	Root string `json:"root,omitempty" yaml:"root,omitempty"`
}

// DaemonControl carries the listener configuration.
type DaemonControl struct {
	// Socket is the unix socket path. Empty disables the unix
	// listener. Default ("") means the daemon auto-selects
	// /var/run/vesseld.sock when neither field is set; the
	// resolver applies that default so DaemonSpec.Validate can
	// stay strictly schema-shape focused.
	Socket string `json:"socket,omitempty" yaml:"socket,omitempty"`

	// Listen is the TCP listen address ("host:port"). Empty
	// disables TCP. Setting Listen REQUIRES Auth.TokenFile; the
	// validator enforces this so an open TCP port without auth
	// can never start.
	Listen string `json:"listen,omitempty" yaml:"listen,omitempty"`

	// Auth carries the authentication knobs. TokenFile is
	// mandatory whenever Listen is set.
	Auth DaemonAuth `json:"auth,omitempty" yaml:"auth,omitempty"`
}

// DaemonAuth holds authentication configuration. Two modes are
// supported and may be combined (defence in depth):
//
//   - TokenFile: shared bearer token in the Authorization header.
//     v0.1.0 default.
//   - MTLS:      mutual TLS — server presents Cert/Key, clients
//     present a cert signed by ClientCA. v0.2.0 addition.
//
// When TCP is enabled (Control.Listen != ""), at least one of the
// two MUST be configured; Validate enforces this so an open TCP
// port without any auth can never start.
type DaemonAuth struct {
	TokenFile string      `json:"tokenFile,omitempty" yaml:"tokenFile,omitempty"`
	MTLS      *DaemonMTLS `json:"mtls,omitempty" yaml:"mtls,omitempty"`
}

// DaemonMTLS pins the server certificate, server key, and the
// trusted client-CA bundle that the TCP listener uses to terminate
// mutual TLS. All three reference fields use the secrets package's
// URL-keyed syntax (env://NAME, file:///abs/path, vault://...) so
// the same indirection that resolves bearer-token files extends to
// cryptographic material.
//
// Setting MTLS is sufficient to authenticate TCP requests — the
// client cert IS the credential. Operators may still configure
// TokenFile alongside it for defence in depth (Authorization header
// is checked after the TLS handshake completes); without an
// MTLS-only mode option, the auth filter stays unchanged.
//
// MinVersion is optional and defaults to TLS 1.3. Operators stuck
// with legacy clients can drop to "1.2" explicitly; older versions
// are intentionally not supported.
type DaemonMTLS struct {
	// Cert is the secret reference to the PEM-encoded server
	// certificate. Required.
	Cert string `json:"cert" yaml:"cert"`

	// Key is the secret reference to the PEM-encoded server
	// private key. Required.
	Key string `json:"key" yaml:"key"`

	// ClientCA is the secret reference to the PEM-encoded trusted
	// client CA bundle. Required — without it the listener would
	// accept any client, which is plain TLS, not mutual TLS.
	ClientCA string `json:"clientCA" yaml:"clientCA"`

	// MinVersion clamps the TLS version negotiated with clients.
	// Empty defaults to "1.3"; "1.2" is the only other accepted
	// value. Anything older is rejected because the protocol
	// itself is no longer considered secure.
	MinVersion string `json:"minVersion,omitempty" yaml:"minVersion,omitempty"`
}

// DaemonResources holds daemon-wide resource caps.
type DaemonResources struct {
	MaxConcurrentRuns int `json:"maxConcurrentRuns,omitempty" yaml:"maxConcurrentRuns,omitempty"`
}

// LLMRateLimit caps requests / tokens per minute against a single
// LLMProfile (referenced by name). PerVesselFairshare divides the
// available budget round-robin across vessels currently competing
// for it; without it the first vessel to wake up wins everything
// until the bucket refills.
type LLMRateLimit struct {
	LLMProfile         string `json:"llmProfile" yaml:"llmProfile"`
	RequestsPerMinute  int    `json:"requestsPerMinute,omitempty" yaml:"requestsPerMinute,omitempty"`
	TokensPerMinute    int    `json:"tokensPerMinute,omitempty" yaml:"tokensPerMinute,omitempty"`
	PerVesselFairshare bool   `json:"perVesselFairshare,omitempty" yaml:"perVesselFairshare,omitempty"`
}

// DaemonLogging controls structured logging output.
type DaemonLogging struct {
	Format string `json:"format,omitempty" yaml:"format,omitempty"` // json | text (default json)
	Level  string `json:"level,omitempty" yaml:"level,omitempty"`   // debug | info | warn | error (default info)
}

// DaemonShutdown holds the SIGTERM grace timeout.
type DaemonShutdown struct {
	DrainTimeout time.Duration `json:"drainTimeout,omitempty" yaml:"drainTimeout,omitempty"`
}

// GetTypeMeta / GetObjectMeta satisfy [Object].
func (d Daemon) GetTypeMeta() TypeMeta     { return d.TypeMeta }
func (d Daemon) GetObjectMeta() ObjectMeta { return d.ObjectMeta }

// Validate runs every shape check that does not require IO. The
// resolver does deeper validation that DOES touch the filesystem
// (e.g. confirming Auth.TokenFile exists); those checks live in
// the resolver because the apispec layer must stay side-effect free.
func (d Daemon) Validate() error {
	if err := d.ObjectMeta.Validate(KindDaemon); err != nil {
		return err
	}
	if d.TypeMeta.APIVersion != APIVersion {
		return errdefs.Validationf("vesseld Daemon %q: apiVersion %q != %q", d.Name, d.TypeMeta.APIVersion, APIVersion)
	}
	if d.TypeMeta.Kind != KindDaemon {
		return errdefs.Validationf("vesseld Daemon %q: kind %q != %q", d.Name, d.TypeMeta.Kind, KindDaemon)
	}
	if d.Spec.Control.Socket == "" && d.Spec.Control.Listen == "" {
		// Resolver will apply the default socket path; validation
		// only flags it as a warning — we accept either explicit
		// "" + resolver default, or any explicit value. The
		// strict check is "do not allow listen without auth".
	}
	if d.Spec.Control.Listen != "" {
		auth := d.Spec.Control.Auth
		if auth.TokenFile == "" && auth.MTLS == nil {
			return errdefs.Validationf(
				"vesseld Daemon %q: spec.control.listen requires at least one of spec.control.auth.tokenFile or spec.control.auth.mtls",
				d.Name)
		}
	}
	if mtls := d.Spec.Control.Auth.MTLS; mtls != nil {
		if mtls.Cert == "" {
			return errdefs.Validationf("vesseld Daemon %q: spec.control.auth.mtls.cert is required", d.Name)
		}
		if mtls.Key == "" {
			return errdefs.Validationf("vesseld Daemon %q: spec.control.auth.mtls.key is required", d.Name)
		}
		if mtls.ClientCA == "" {
			return errdefs.Validationf("vesseld Daemon %q: spec.control.auth.mtls.clientCA is required (without it the listener accepts any client → not mutual TLS)", d.Name)
		}
		switch mtls.MinVersion {
		case "", "1.2", "1.3":
		default:
			return errdefs.Validationf("vesseld Daemon %q: spec.control.auth.mtls.minVersion %q invalid (want 1.2|1.3)", d.Name, mtls.MinVersion)
		}
	}
	if d.Spec.Resources.MaxConcurrentRuns < 0 {
		return errdefs.Validationf("vesseld Daemon %q: spec.resources.maxConcurrentRuns must be >= 0", d.Name)
	}
	for i, rl := range d.Spec.LLMRateLimits {
		if rl.LLMProfile == "" {
			return errdefs.Validationf("vesseld Daemon %q: spec.llmRateLimits[%d].llmProfile is required", d.Name, i)
		}
		if rl.RequestsPerMinute < 0 || rl.TokensPerMinute < 0 {
			return errdefs.Validationf("vesseld Daemon %q: spec.llmRateLimits[%d] caps must be >= 0", d.Name, i)
		}
	}
	switch d.Spec.Logging.Format {
	case "", "json", "text":
	default:
		return errdefs.Validationf("vesseld Daemon %q: spec.logging.format %q invalid (want json|text)", d.Name, d.Spec.Logging.Format)
	}
	switch d.Spec.Logging.Level {
	case "", "debug", "info", "warn", "error":
	default:
		return errdefs.Validationf("vesseld Daemon %q: spec.logging.level %q invalid (want debug|info|warn|error)", d.Name, d.Spec.Logging.Level)
	}
	if d.Spec.Shutdown.DrainTimeout < 0 {
		return errdefs.Validationf("vesseld Daemon %q: spec.shutdown.drainTimeout must be >= 0", d.Name)
	}
	if ss := d.Spec.SessionStore; ss != nil {
		switch ss.Backend {
		case "memory":
			if ss.Root != "" {
				return errdefs.Validationf("vesseld Daemon %q: spec.sessionStore.root must be empty when backend=memory", d.Name)
			}
		case "filesystem":
			if ss.Root == "" {
				return errdefs.Validationf("vesseld Daemon %q: spec.sessionStore.root is required when backend=filesystem", d.Name)
			}
		case "":
			return errdefs.Validationf("vesseld Daemon %q: spec.sessionStore.backend is required when sessionStore is set (memory|filesystem)", d.Name)
		default:
			return errdefs.Validationf("vesseld Daemon %q: spec.sessionStore.backend %q invalid (want memory|filesystem)", d.Name, ss.Backend)
		}
	}
	return nil
}
