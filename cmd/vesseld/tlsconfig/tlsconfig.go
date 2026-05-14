package tlsconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/GizClaw/flowcraft/cmd/vesseld/secrets"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Config bundles the references that point at the PEM material
// the daemon will load. The strings are passed through to the
// [secrets.Provider] verbatim; format follows whatever schemes the
// provider supports (env://, file:///abs/path, vault://...).
type Config struct {
	// CertRef points at the server certificate (PEM). Required.
	CertRef string

	// KeyRef points at the server private key (PEM). Required.
	KeyRef string

	// ClientCARef points at the trusted client CA bundle (PEM).
	// Required — without it the listener would accept any client,
	// defeating the purpose of mutual TLS.
	ClientCARef string

	// MinVersion is "1.2" or "1.3". Empty defaults to "1.3".
	MinVersion string
}

// Build resolves every reference through provider and assembles a
// [crypto/tls.Config] suitable for an mTLS HTTP listener. The
// returned config has ClientAuth = RequireAndVerifyClientCert so a
// handshake without a client cert (or with one not chained to
// ClientCARef) is rejected at the TLS layer — the request never
// reaches the HTTP mux.
//
// Errors are classified:
//
//   - errdefs.Validation: cfg is incomplete (missing ref, unsupported
//     MinVersion) or the resolved bytes are not parseable as PEM.
//   - errdefs.NotFound / errdefs.NotAvailable / errdefs.Forbidden:
//     surfaced from the underlying provider; Build preserves the
//     classification so the daemon's startup path can map them to
//     the same exit codes as plain secret resolution.
func Build(ctx context.Context, provider secrets.Provider, cfg Config) (*tls.Config, error) {
	if provider == nil {
		return nil, errdefs.Validationf("tlsconfig: provider is required")
	}
	if cfg.CertRef == "" {
		return nil, errdefs.Validationf("tlsconfig: CertRef is required")
	}
	if cfg.KeyRef == "" {
		return nil, errdefs.Validationf("tlsconfig: KeyRef is required")
	}
	if cfg.ClientCARef == "" {
		return nil, errdefs.Validationf("tlsconfig: ClientCARef is required")
	}

	minVersion, err := mapMinVersion(cfg.MinVersion)
	if err != nil {
		return nil, err
	}

	certPEM, err := provider.Get(ctx, cfg.CertRef)
	if err != nil {
		return nil, fmt.Errorf("tlsconfig: resolve cert %q: %w", cfg.CertRef, err)
	}
	keyPEM, err := provider.Get(ctx, cfg.KeyRef)
	if err != nil {
		return nil, fmt.Errorf("tlsconfig: resolve key %q: %w", cfg.KeyRef, err)
	}
	caPEM, err := provider.Get(ctx, cfg.ClientCARef)
	if err != nil {
		return nil, fmt.Errorf("tlsconfig: resolve clientCA %q: %w", cfg.ClientCARef, err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// X509KeyPair surfaces "PEM block not parseable" /
		// "key does not match cert" as generic errors. Either
		// case is a config-shape problem and the operator must
		// fix it before the daemon can start, so Validation is
		// the right classification.
		return nil, errdefs.Validationf("tlsconfig: load X509 key pair: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errdefs.Validationf("tlsconfig: clientCA %q contains no parseable PEM blocks", cfg.ClientCARef)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		// RequireAndVerifyClientCert is the whole point of this
		// package. RequestClientCert / VerifyClientCertIfGiven
		// would silently accept anonymous clients on a TLS
		// listener and that is the exact "thought we had mTLS,
		// didn't" footgun this package exists to prevent.
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: minVersion,
	}, nil
}

// mapMinVersion translates the human-readable string accepted by
// the apispec into the tls.VersionTLS{12,13} constants. Anything
// outside that allow-list is rejected — older protocols are no
// longer considered secure and we refuse to start the listener.
func mapMinVersion(s string) (uint16, error) {
	switch s {
	case "", "1.3":
		return tls.VersionTLS13, nil
	case "1.2":
		return tls.VersionTLS12, nil
	default:
		return 0, errdefs.Validationf("tlsconfig: MinVersion %q invalid (want 1.2|1.3)", s)
	}
}
