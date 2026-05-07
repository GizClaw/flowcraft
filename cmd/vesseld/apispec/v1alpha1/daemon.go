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

// DaemonAuth holds authentication configuration. Token-file is the
// only mode in v0.1.0; mTLS / OIDC are explicitly v0.2.0+ scope.
type DaemonAuth struct {
	TokenFile string `json:"tokenFile,omitempty" yaml:"tokenFile,omitempty"`
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
	if d.Spec.Control.Listen != "" && d.Spec.Control.Auth.TokenFile == "" {
		return errdefs.Validationf("vesseld Daemon %q: spec.control.listen requires spec.control.auth.tokenFile", d.Name)
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
	return nil
}
