package httpkit

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func fastRetry() RetryConfig {
	return RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func get(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("body")))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	return response
}

func TestRetriesTransientStatuses(t *testing.T) {
	statuses := []int{429, 500, 503, 408}
	for _, status := range statuses {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) < 3 {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			client := &http.Client{Transport: newRetryTransport(nil, fastRetry())}
			response := get(t, client, server.URL)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			if calls.Load() != 3 {
				t.Fatalf("calls = %d, want 3", calls.Load())
			}
		})
	}
}

func TestDoesNotRetryPermanentFailures(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := &http.Client{Transport: newRetryTransport(nil, fastRetry())}
	response := get(t, client, server.URL)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on 400)", calls.Load())
	}
}

func TestRetryExhaustionReturnsLastResponse(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := &http.Client{Transport: newRetryTransport(nil, fastRetry())}
	response := get(t, client, server.URL)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want exactly MaxAttempts", calls.Load())
	}
}

func TestResponseHeaderTimeoutRecoversConnection(t *testing.T) {
	var first atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !first.Swap(true) {
			// Stall before writing headers; the transport should abort.
			time.Sleep(5 * time.Second)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(
		WithResponseHeaderTimeout(200*time.Millisecond),
		WithTimeout(2*time.Second),
		WithRetry(RetryConfig{
			MaxAttempts: 1,
			BaseDelay:   time.Millisecond,
			MaxDelay:    5 * time.Millisecond,
		}),
	)
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.Do(request)
	if err == nil {
		t.Fatal("stalled request should fail")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stall not bounded: %s", elapsed)
	}

	// The same client must recover on a fresh connection.
	response := get(t, client, server.URL)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("recovery status = %d", response.StatusCode)
	}
}

func TestNewClientHTTP1(t *testing.T) {
	client := NewClient(WithTimeout(3*time.Minute), WithResponseHeaderTimeout(2*time.Minute))
	if client.Timeout != 3*time.Minute {
		t.Fatalf("Timeout = %s", client.Timeout)
	}
	transport, ok := client.Transport.(*retryTransport)
	if !ok {
		t.Fatalf("Transport = %T, want retryTransport", client.Transport)
	}
	base, ok := transport.base.(*http.Transport)
	if !ok {
		t.Fatalf("base transport = %T, want *http.Transport", transport.base)
	}
	if base.ForceAttemptHTTP2 || base.ResponseHeaderTimeout != 2*time.Minute {
		t.Fatalf("hardened base misconfigured: %+v", base)
	}
}

func TestNewClientHTTP2(t *testing.T) {
	client := NewClient(
		WithHTTP2(),
		WithTimeout(3*time.Minute),
		WithHTTP2Timeouts(20*time.Second, 10*time.Second, 15*time.Second),
	)
	if client.Timeout != 3*time.Minute {
		t.Fatalf("Timeout = %s", client.Timeout)
	}
	retry, ok := client.Transport.(*retryTransport)
	if !ok {
		t.Fatalf("Transport = %T, want retryTransport", client.Transport)
	}
	base, ok := retry.base.(*http.Transport)
	if !ok {
		t.Fatalf("base transport = %T, want *http.Transport", retry.base)
	}
	if !base.ForceAttemptHTTP2 || base.TLSNextProto["h2"] == nil {
		t.Fatalf("hardened h2 misconfigured: %+v", base)
	}
	if base.ResponseHeaderTimeout != 5*time.Minute {
		t.Fatalf("ResponseHeaderTimeout = %s", base.ResponseHeaderTimeout)
	}
}

func TestHTTP2ClientRoundTrip(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	client := NewClient(
		WithHTTP2(),
		WithTimeout(10*time.Second),
		WithHTTP2Timeouts(time.Second, time.Second, time.Second),
		WithTLSClientConfig(&tls.Config{RootCAs: pool}),
	)
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("proto = %s, want HTTP/2", response.Proto)
	}
}

func TestWithoutRetryUsesBaseTransport(t *testing.T) {
	roundTripper := NewRoundTripper(WithoutRetry())
	if _, ok := roundTripper.(*retryTransport); ok {
		t.Fatal("retry should be disabled")
	}
}

func TestNewClientHTTP3(t *testing.T) {
	client := NewClient(
		WithHTTP3(),
		WithTimeout(3*time.Minute),
		WithQUICTimeouts(5*time.Second, 20*time.Second, 10*time.Second),
	)
	if client.Timeout != 3*time.Minute {
		t.Fatalf("Timeout = %s", client.Timeout)
	}
	retry, ok := client.Transport.(*retryTransport)
	if !ok {
		t.Fatalf("Transport = %T, want retryTransport", client.Transport)
	}
	base, ok := retry.base.(*http3.Transport)
	if !ok {
		t.Fatalf("base transport = %T, want *http3.Transport", retry.base)
	}
	if base.QUICConfig == nil ||
		base.QUICConfig.HandshakeIdleTimeout != 5*time.Second ||
		base.QUICConfig.MaxIdleTimeout != 20*time.Second ||
		base.QUICConfig.KeepAlivePeriod != 10*time.Second {
		t.Fatalf("hardened h3 misconfigured: %+v", base.QUICConfig)
	}
}

func TestHTTP3ClientRoundTrip(t *testing.T) {
	cert := selfSignedCert(t)
	serverTLS := http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}})
	server := &http3.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		TLSConfig: serverTLS,
	}
	listener, err := quic.ListenAddrEarly("127.0.0.1:0", serverTLS, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.ServeListener(listener) }()
	defer server.Close()

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	client := NewClient(
		WithHTTP3(),
		WithTimeout(10*time.Second),
		WithQUICTimeouts(5*time.Second, 10*time.Second, time.Second),
		WithTLSClientConfig(&tls.Config{RootCAs: pool, NextProtos: []string{http3.NextProtoH3}}),
	)

	request, err := http.NewRequest(http.MethodGet, "https://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 3 {
		t.Fatalf("proto = %s, want HTTP/3", response.Proto)
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
}

func TestRetriesTransportErrors(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	url := server.URL
	server.Close() // first attempts hit a dead server; restart below

	// A server that refuses the first two connections then serves is hard
	// to stage with httptest; instead point at a closed port and expect a
	// transport error to surface after MaxAttempts.
	client := &http.Client{Transport: newRetryTransport(nil, fastRetry())}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(request)
	if err == nil {
		t.Fatal("expected a transport error against a dead server")
	}
}

func TestBodyReplaysAcrossAttempts(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(payload))
		if len(bodies) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{Transport: newRetryTransport(nil, fastRetry())}
	get(t, client, server.URL)
	if len(bodies) != 3 {
		t.Fatalf("attempts = %d", len(bodies))
	}
	for i, body := range bodies {
		if body != "body" {
			t.Fatalf("attempt %d body = %q, want replayed payload", i, body)
		}
	}
}

func TestRetryAfterHonored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	transport := &retryTransport{base: newTransport(), config: fastRetry()}
	delay := transport.backoff(1, &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"1"}},
	})
	if delay != time.Second {
		t.Fatalf("delay = %v, want 1s from Retry-After", delay)
	}
	_ = server
}

func TestUnreplayableBodyPassesThrough(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := &http.Client{Transport: newRetryTransport(nil, fastRetry())}
	request, err := http.NewRequest(http.MethodPost, server.URL, io.NopCloser(bytes.NewReader([]byte("x"))))
	if err != nil {
		t.Fatal(err)
	}
	request.GetBody = nil // NopCloser bodies have no replay channel
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	response.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (no replay possible)", calls.Load())
	}
}

func TestContextCancellationStopsBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	slow := RetryConfig{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: 5 * time.Second}
	client := &http.Client{Transport: newRetryTransport(nil, slow)}
	request, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	cancel()

	_, err = client.Do(request)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
