package tlsconfig

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// fakeProvider implements secrets.Provider for tests. The map is
// ref → bytes; an unknown ref returns errdefs.NotFound so we can
// exercise both the happy path and the surfacing of provider
// errors.
type fakeProvider struct {
	data map[string][]byte
}

func (f *fakeProvider) Get(_ context.Context, ref string) ([]byte, error) {
	v, ok := f.data[ref]
	if !ok {
		return nil, errdefs.NotFoundf("test: ref %q not registered", ref)
	}
	return v, nil
}

// genPair returns a freshly minted self-signed cert + private key
// (PEM-encoded) suitable for both server and CA roles in unit
// tests. The cert is valid for one hour starting now — long enough
// to outlive any reasonable test run, short enough that a leaked
// cert is meaningless an hour later.
func genPair(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestBuild_HappyPath(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := genPair(t, "vesseld-test")
	caPEM, _ := genPair(t, "client-ca")

	p := &fakeProvider{data: map[string][]byte{
		"file:///cert": certPEM,
		"file:///key":  keyPEM,
		"file:///ca":   caPEM,
	}}
	out, err := Build(context.Background(), p, Config{
		CertRef:     "file:///cert",
		KeyRef:      "file:///key",
		ClientCARef: "file:///ca",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := out.MinVersion; got != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %d, want %d (TLS 1.3)", got, tls.VersionTLS13)
	}
	if out.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", out.ClientAuth)
	}
	if len(out.Certificates) != 1 {
		t.Fatalf("Certificates len = %d, want 1", len(out.Certificates))
	}
	if out.ClientCAs == nil {
		t.Fatal("ClientCAs is nil")
	}
}

func TestBuild_MinVersion12Honoured(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := genPair(t, "vesseld-test")
	caPEM, _ := genPair(t, "client-ca")
	p := &fakeProvider{data: map[string][]byte{
		"c": certPEM, "k": keyPEM, "a": caPEM,
	}}
	out, err := Build(context.Background(), p, Config{
		CertRef: "c", KeyRef: "k", ClientCARef: "a", MinVersion: "1.2",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want %d", out.MinVersion, tls.VersionTLS12)
	}
}

func TestBuild_RejectsInvalidMinVersion(t *testing.T) {
	t.Parallel()
	p := &fakeProvider{data: map[string][]byte{}}
	_, err := Build(context.Background(), p, Config{
		CertRef: "c", KeyRef: "k", ClientCARef: "a", MinVersion: "1.1",
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestBuild_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	p := &fakeProvider{}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no-cert", Config{KeyRef: "k", ClientCARef: "a"}},
		{"no-key", Config{CertRef: "c", ClientCARef: "a"}},
		{"no-ca", Config{CertRef: "c", KeyRef: "k"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Build(context.Background(), p, tc.cfg); !errdefs.IsValidation(err) {
				t.Fatalf("expected Validation, got %v", err)
			}
		})
	}
}

func TestBuild_RejectsNilProvider(t *testing.T) {
	t.Parallel()
	if _, err := Build(context.Background(), nil, Config{CertRef: "c", KeyRef: "k", ClientCARef: "a"}); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestBuild_PropagatesProviderError(t *testing.T) {
	t.Parallel()
	p := &fakeProvider{data: map[string][]byte{}}
	_, err := Build(context.Background(), p, Config{
		CertRef: "missing", KeyRef: "k", ClientCARef: "a",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errdefs.IsNotFound(err) {
		t.Fatalf("expected wrapped NotFound, got %v", err)
	}
}

func TestBuild_RejectsBadCertPEM(t *testing.T) {
	t.Parallel()
	_, keyPEM := genPair(t, "vesseld-test")
	caPEM, _ := genPair(t, "ca")
	p := &fakeProvider{data: map[string][]byte{
		"c": []byte("not pem"), "k": keyPEM, "a": caPEM,
	}}
	_, err := Build(context.Background(), p, Config{
		CertRef: "c", KeyRef: "k", ClientCARef: "a",
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestBuild_RejectsBadCAPEM(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := genPair(t, "vesseld-test")
	p := &fakeProvider{data: map[string][]byte{
		"c": certPEM, "k": keyPEM, "a": []byte("not pem"),
	}}
	_, err := Build(context.Background(), p, Config{
		CertRef: "c", KeyRef: "k", ClientCARef: "a",
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestBuild_RejectsKeyCertMismatch(t *testing.T) {
	t.Parallel()
	certPEM, _ := genPair(t, "vesseld-test")
	_, otherKeyPEM := genPair(t, "other")
	caPEM, _ := genPair(t, "ca")
	p := &fakeProvider{data: map[string][]byte{
		"c": certPEM, "k": otherKeyPEM, "a": caPEM,
	}}
	_, err := Build(context.Background(), p, Config{
		CertRef: "c", KeyRef: "k", ClientCARef: "a",
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}
