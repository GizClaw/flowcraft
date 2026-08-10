package mitm

import (
	"os"
	"path/filepath"
)

// systemBundleCandidates are the common system CA bundle locations on
// Linux and macOS. The first existing file is copied into the merged
// bundle so SSL_CERT_FILE still trusts the platform's roots.
var systemBundleCandidates = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// WriteBundle writes the temporary MITM CA alongside a copy of the
// system roots into a fresh 0600 temp file, and returns its path plus
// a cleanup function. The path is meant to be bound into the sandbox
// and referenced by SSL_CERT_FILE.
func WriteBundle(caPEM []byte) (string, func(), error) {
	dir, err := os.MkdirTemp("", "flowcraft-ca-bundle-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "ca-bundle.pem")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	for _, candidate := range systemBundleCandidates {
		data, readErr := os.ReadFile(candidate)
		if readErr != nil {
			continue
		}
		_, _ = f.Write(data)
		_, _ = f.Write([]byte("\n"))
	}
	_, _ = f.Write(caPEM)
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
