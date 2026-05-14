package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

// keyPair is a self-signed cert + private key bundle used by the
// mTLS round-trip test. We mint everything in-process so the test
// has no filesystem dependencies and no risk of stale fixtures.
type keyPair struct {
	cert     *x509.Certificate
	certDER  []byte
	tlsCert  tls.Certificate
	privKey  *ecdsa.PrivateKey
	template *x509.Certificate
}

func mintCA(t *testing.T, cn string) *keyPair {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate (CA): %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return &keyPair{cert: parsed, certDER: der, privKey: priv, template: tmpl}
}

// signLeaf issues a leaf cert chained to ca. The serverDNS list,
// when non-empty, populates SANs so the test client can verify the
// server certificate with the standard hostname check.
func signLeaf(t *testing.T, ca *keyPair, cn string, eku []x509.ExtKeyUsage, serverDNS []string) tls.Certificate {
	t.Helper()
	leafPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  eku,
		DNSNames:     serverDNS,
	}
	// Stamp loopback IPs as SANs when the leaf is meant to serve
	// requests — the test client dials 127.0.0.1, and the stdlib
	// hostname verifier matches the connect target against IPAddresses
	// (not DNSNames) when the target is a literal IP. Without this,
	// the verifier returns "doesn't contain any IP SANs".
	for _, name := range serverDNS {
		if ip := net.ParseIP(name); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &leafPriv.PublicKey, ca.privKey)
	if err != nil {
		t.Fatalf("CreateCertificate (leaf): %v", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  leafPriv,
		Leaf:        mustParse(t, der),
	}
}

func mustParse(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return c
}

// TestAPI_TCPMTLSHandshake spins up a real TCP listener wrapped in
// tls.Config{ClientAuth: RequireAndVerifyClientCert}, then exercises
// three cases:
//
//  1. Client presents a cert signed by the expected CA → 200.
//  2. Client presents NO cert                          → handshake error.
//  3. Client presents cert from an unknown CA          → handshake error.
//
// Together they pin the contract that mTLS is the auth boundary —
// not "we accepted a TLS connection and then checked tokens", but
// "we refused to accept a connection without a valid client cert".
func TestAPI_TCPMTLSHandshake(t *testing.T) {
	t.Parallel()

	serverCA := mintCA(t, "server-ca")
	clientCA := mintCA(t, "client-ca")
	otherCA := mintCA(t, "other-ca")

	serverCert := signLeaf(t, serverCA, "localhost",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		[]string{"localhost", "127.0.0.1"})
	goodClientCert := signLeaf(t, clientCA, "good-client",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	rogueClientCert := signLeaf(t, otherCA, "rogue-client",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)

	// Trust pool the server uses to verify clients.
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(clientCA.cert)
	// Trust pool the client uses to verify the server.
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(serverCA.cert)

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}

	// Bind to localhost:0 so the kernel picks a free port. Setting
	// cfg.Listen to the returned address completes the wiring of
	// Server.Start → net.Listen("tcp", cfg.Listen) →
	// tls.NewListener(rawL, cfg.TLS). We can't reuse newTestServer
	// because it would call MarkReady on a stale config.
	s := newTestServer(t)
	s.cfg.Listen = "127.0.0.1:0"
	s.cfg.TLS = serverTLS
	// Token MUST be left empty here: the test exercises the "mTLS
	// alone is sufficient auth" branch in Server.Start. If Token
	// were non-empty, authn would also require a Bearer header.
	s.cfg.Token = ""

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = s.Stop(stopCtx)
	})

	// Resolve the kernel-assigned port off the listener. The TCP
	// listener is the SECOND entry when both socket and listen are
	// configured (socket is registered first in Start); the test
	// config above does not set Socket, so listeners[0] is the TLS
	// listener directly.
	if len(s.listeners) != 1 {
		t.Fatalf("expected 1 listener, got %d", len(s.listeners))
	}
	addr := s.listeners[0].Addr().String()
	url := "https://" + addr + "/healthz"

	// Case 1: valid client cert → 200 ok.
	clientGood := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      rootCAs,
				Certificates: []tls.Certificate{goodClientCert},
			},
		},
		Timeout: 5 * time.Second,
	}
	resp, err := clientGood.Get(url)
	if err != nil {
		t.Fatalf("good client GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good client status=%d body=%s", resp.StatusCode, body)
	}

	// Case 2: no client cert → handshake fails. Different Go
	// versions surface this as different error strings; we just
	// assert any error.
	clientNone := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: rootCAs},
		},
		Timeout: 5 * time.Second,
	}
	if _, err := clientNone.Get(url); err == nil {
		t.Fatal("expected handshake error without client cert, got nil")
	}

	// Case 3: rogue client cert → handshake fails.
	clientRogue := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      rootCAs,
				Certificates: []tls.Certificate{rogueClientCert},
			},
		},
		Timeout: 5 * time.Second,
	}
	if _, err := clientRogue.Get(url); err == nil {
		t.Fatal("expected handshake error with rogue client cert, got nil")
	}
}

// TestAPI_StartRequiresAuthOnTCP pins the auth-boundary check: a
// Server with Listen set but neither Token nor TLS must refuse to
// Start. The error string travels back to runtime where it surfaces
// as a startup failure with a 1 exit code — losing this check would
// silently start an open TCP port.
func TestAPI_StartRequiresAuthOnTCP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	s.cfg.Listen = "127.0.0.1:0"
	s.cfg.Token = ""
	s.cfg.TLS = nil
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected error, got nil")
	}
}
