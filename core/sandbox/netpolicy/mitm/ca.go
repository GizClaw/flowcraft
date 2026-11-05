package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"time"
)

// caValidity is how long the ephemeral root CA lives. Leaves are
// issued for slightly less so they never outlive their signer.
const caValidity = 24 * time.Hour

// maxCachedLeaves bounds the per-CA leaf cache. A long-running proxy
// that sees many distinct hostnames must not grow this map without
// bound; eviction is FIFO, which is enough for a cache whose entries
// are cheap to regenerate.
const maxCachedLeaves = 1024

// CA is a per-run temporary certificate authority. The private key
// lives only in memory and is discarded when the CA is garbage
// collected; leaf certificates are cached per host.
type CA struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	pem    []byte
	mu     sync.Mutex
	leaves map[string]*tls.Certificate
	order  []string
}

// NewCA generates a fresh self-signed ECDSA P-256 root CA.
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "flowcraft netproxy temporary CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("mitm: create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("mitm: parse CA certificate: %w", err)
	}
	return &CA{
		cert:   cert,
		key:    key,
		pem:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leaves: make(map[string]*tls.Certificate),
	}, nil
}

// PEM returns the root CA certificate in PEM form for bundle
// injection into the sandbox.
func (c *CA) PEM() []byte { return append([]byte(nil), c.pem...) }

// Leaf returns (and caches) a leaf certificate whose SAN is host —
// a DNS SAN for hostnames, an IP SAN for literals.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[host]; ok {
		return leaf, nil
	}
	leaf, err := c.issueLeaf(host)
	if err != nil {
		return nil, err
	}
	if len(c.leaves) >= maxCachedLeaves {
		delete(c.leaves, c.order[0])
		c.order = c.order[1:]
	}
	c.leaves[host] = leaf
	c.order = append(c.order, host)
	return leaf, nil
}

func (c *CA) issueLeaf(host string) (*tls.Certificate, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(caValidity - time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		tmpl.IPAddresses = []net.IP{net.IP(ip.AsSlice())}
	} else {
		tmpl.DNSNames = []string{host}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mitm: generate leaf key: %w", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("mitm: issue leaf for %q: %w", host, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  key,
		Leaf:        tmpl,
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("mitm: random serial: %w", err)
	}
	return serial, nil
}
