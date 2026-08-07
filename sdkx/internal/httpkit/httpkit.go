// Package httpkit carries the HTTP conveniences the hand-rolled provider
// clients (qwen, minimax, kimi, bytedance) use: a tuned connection-pooling
// transport, a bounded retry RoundTripper for transient failures, and an
// option-driven client builder that hardens HTTP/1.1, HTTP/2, or HTTP/3.
package httpkit

import (
	"crypto/tls"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

// Protocol selects the transport family built by [NewRoundTripper].
type Protocol int

const (
	// ProtocolHTTP1 is the default: HTTP/1.1 with ResponseHeaderTimeout,
	// so a stalled connection is bounded and retried on a fresh one.
	ProtocolHTTP1 Protocol = iota
	// ProtocolHTTP2 keeps multiplexing and bounds the write path with
	// periodic PING health checks and WriteByteTimeout.
	ProtocolHTTP2
	// ProtocolHTTP3 runs HTTP over QUIC (UDP). Streams are independent at
	// the transport level and QUIC idle timeouts detect dead peers.
	ProtocolHTTP3
)

// Config carries every knob for [NewRoundTripper] / [NewClient]. Zero values
// select the defaults below.
type Config struct {
	Protocol Protocol

	// ClientTimeout bounds the whole request (http.Client.Timeout).
	ClientTimeout time.Duration

	// TLSClientConfig is used for TLS dialing. Nil uses system roots.
	TLSClientConfig *tls.Config

	// ResponseHeaderTimeout bounds the wait for HTTP/1.1 response headers.
	ResponseHeaderTimeout time.Duration

	// HTTP/2 health checks.
	PingInterval     time.Duration // quiet-connection PING cadence
	PingTimeout      time.Duration // close if a PING goes unanswered
	WriteByteTimeout time.Duration // close if the write path blocks

	// HTTP/3 (QUIC) connection timeouts.
	QUICHandshakeIdleTimeout time.Duration
	QUICMaxIdleTimeout       time.Duration
	QUICKeepAlivePeriod      time.Duration

	// Connection pooling (HTTP/1.1).
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration

	// Retry wraps the base transport; disabled with WithoutRetry.
	Retry        RetryConfig
	RetryEnabled bool
}

// DefaultConfig returns the recommended hardened defaults.
func DefaultConfig() Config {
	return Config{
		Protocol:                 ProtocolHTTP1,
		ClientTimeout:            10 * time.Minute,
		ResponseHeaderTimeout:    5 * time.Minute,
		PingInterval:             30 * time.Second,
		PingTimeout:              15 * time.Second,
		WriteByteTimeout:         30 * time.Second,
		QUICHandshakeIdleTimeout: 10 * time.Second,
		QUICMaxIdleTimeout:       30 * time.Second,
		QUICKeepAlivePeriod:      15 * time.Second,
		MaxIdleConns:             128,
		MaxIdleConnsPerHost:      32,
		IdleConnTimeout:          90 * time.Second,
		Retry:                    DefaultRetry,
		RetryEnabled:             true,
	}
}

// Option mutates a Config.
type Option func(*Config)

func WithProtocol(protocol Protocol) Option {
	return func(config *Config) { config.Protocol = protocol }
}

// WithHTTP1 selects the hardened HTTP/1.1 transport.
func WithHTTP1() Option { return WithProtocol(ProtocolHTTP1) }

// WithHTTP2 selects the hardened HTTP/2 transport.
func WithHTTP2() Option { return WithProtocol(ProtocolHTTP2) }

// WithHTTP3 selects the QUIC-based HTTP/3 transport.
func WithHTTP3() Option { return WithProtocol(ProtocolHTTP3) }

func WithTimeout(timeout time.Duration) Option {
	return func(config *Config) { config.ClientTimeout = timeout }
}

// WithTLSClientConfig supplies the TLS configuration used for dialing
// (custom CA roots, client certificates, server name overrides).
func WithTLSClientConfig(config *tls.Config) Option {
	return func(cfg *Config) { cfg.TLSClientConfig = config }
}

func WithResponseHeaderTimeout(timeout time.Duration) Option {
	return func(config *Config) { config.ResponseHeaderTimeout = timeout }
}

// WithHTTP2Timeouts sets the HTTP/2 health-check knobs.
func WithHTTP2Timeouts(pingInterval, pingTimeout, writeByteTimeout time.Duration) Option {
	return func(config *Config) {
		config.PingInterval = pingInterval
		config.PingTimeout = pingTimeout
		config.WriteByteTimeout = writeByteTimeout
	}
}

// WithQUICTimeouts sets the HTTP/3 connection timeouts: QUIC handshake,
// connection idle timeout, and keep-alive period for dead-peer detection.
func WithQUICTimeouts(handshakeIdleTimeout, maxIdleTimeout, keepAlivePeriod time.Duration) Option {
	return func(config *Config) {
		config.QUICHandshakeIdleTimeout = handshakeIdleTimeout
		config.QUICMaxIdleTimeout = maxIdleTimeout
		config.QUICKeepAlivePeriod = keepAlivePeriod
	}
}

func WithRetry(config RetryConfig) Option {
	return func(cfg *Config) {
		cfg.Retry = config
		cfg.RetryEnabled = true
	}
}

func WithoutRetry() Option {
	return func(config *Config) { config.RetryEnabled = false }
}

// WithConnectionPool tunes HTTP/1.1 keep-alive pooling.
func WithConnectionPool(maxIdleConns, maxIdleConnsPerHost int, idleConnTimeout time.Duration) Option {
	return func(config *Config) {
		config.MaxIdleConns = maxIdleConns
		config.MaxIdleConnsPerHost = maxIdleConnsPerHost
		config.IdleConnTimeout = idleConnTimeout
	}
}

// NewRoundTripper builds the hardened base transport per Config and wraps it
// with the retry transport unless disabled.
func NewRoundTripper(options ...Option) http.RoundTripper {
	config := applyOptions(options)
	base := buildBaseTransport(config)
	if !config.RetryEnabled {
		return base
	}
	return newRetryTransport(base, config.Retry)
}

// NewClient builds an http.Client over [NewRoundTripper] with the configured
// whole-request timeout.
func NewClient(options ...Option) *http.Client {
	config := applyOptions(options)
	return &http.Client{
		Transport: NewRoundTripper(options...),
		Timeout:   config.ClientTimeout,
	}
}

func applyOptions(options []Option) Config {
	config := DefaultConfig()
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	return config
}

func buildBaseTransport(config Config) http.RoundTripper {
	switch config.Protocol {
	case ProtocolHTTP3:
		return &http3.Transport{
			TLSClientConfig: config.TLSClientConfig,
			QUICConfig: &quic.Config{
				HandshakeIdleTimeout: config.QUICHandshakeIdleTimeout,
				MaxIdleTimeout:       config.QUICMaxIdleTimeout,
				KeepAlivePeriod:      config.QUICKeepAlivePeriod,
			},
		}
	case ProtocolHTTP2:
		http1 := newTransport()
		http1.ResponseHeaderTimeout = config.ResponseHeaderTimeout
		http1.TLSClientConfig = config.TLSClientConfig
		h2, err := http2.ConfigureTransports(http1)
		if err != nil {
			panic("httpkit: configure h2: " + err.Error())
		}
		h2.ReadIdleTimeout = config.PingInterval
		h2.PingTimeout = config.PingTimeout
		h2.WriteByteTimeout = config.WriteByteTimeout
		h2.IdleConnTimeout = config.IdleConnTimeout
		return http1
	case ProtocolHTTP1:
		transport := newTransport()
		transport.ForceAttemptHTTP2 = false
		transport.ResponseHeaderTimeout = config.ResponseHeaderTimeout
		transport.TLSClientConfig = config.TLSClientConfig
		return transport
	default:
		panic("httpkit: unknown protocol")
	}
}

// newTransport returns a connection-pooling base transport tuned for a
// small number of provider hosts: many idle keep-alive connections per
// host so concurrent streams do not redial.
func newTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 128
	transport.MaxIdleConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second
	return transport
}

// RetryConfig bounds the retry behaviour.
type RetryConfig struct {
	// MaxAttempts is the total number of tries including the first.
	MaxAttempts int
	// BaseDelay seeds the exponential backoff.
	BaseDelay time.Duration
	// MaxDelay caps one backoff sleep (a Retry-After hint may exceed it).
	MaxDelay time.Duration
}

// DefaultRetry retries transient failures twice after the first attempt
// with a 200ms-seeded exponential backoff.
var DefaultRetry = RetryConfig{
	MaxAttempts: 3,
	BaseDelay:   200 * time.Millisecond,
	MaxDelay:    2 * time.Second,
}

// newRetryTransport decorates base with bounded retries for transient
// failures: network errors, 408, 429, and 5xx responses. The wrapper only
// engages while a request is replayable (net/http sets GetBody for the
// byte-slice bodies the provider clients send), so a streaming request
// retries connection and status failures but never re-opens mid-stream.
// Retry-After hints from 429/503 responses are honored.
func newRetryTransport(base http.RoundTripper, config RetryConfig) http.RoundTripper {
	if base == nil {
		base = newTransport()
	}
	if config.MaxAttempts < 1 {
		config.MaxAttempts = 1
	}
	return &retryTransport{base: base, config: config}
}

type retryTransport struct {
	base   http.RoundTripper
	config RetryConfig
}

func (t *retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// Without GetBody the body cannot be replayed: one attempt, exactly
	// like a bare transport.
	if request.Body != nil && request.GetBody == nil {
		return t.base.RoundTrip(request)
	}

	var (
		response *http.Response
		err      error
	)
	for attempt := 1; ; attempt++ {
		attemptRequest := request.Clone(request.Context())
		if request.Body != nil {
			body, bodyErr := request.GetBody()
			if bodyErr != nil {
				return nil, bodyErr
			}
			attemptRequest.Body = body
		}

		response, err = t.base.RoundTrip(attemptRequest)
		if attempt >= t.config.MaxAttempts || !retryable(response, err) {
			return response, err
		}
		if response != nil && response.Body != nil {
			// Drain so the pooled connection can be reused.
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4*1024))
			_ = response.Body.Close()
		}

		delay := t.backoff(attempt, response)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-request.Context().Done():
			timer.Stop()
			return nil, request.Context().Err()
		}
	}
}

// retryable reports whether the failure is transient: a transport error
// (dial reset, EOF, timeout — context cancellation is the caller's own
// doing and never retries) or a throttling/server status.
func retryable(response *http.Response, err error) bool {
	if err != nil {
		if strings.Contains(err.Error(), "context canceled") ||
			strings.Contains(err.Error(), "context deadline exceeded") {
			return false
		}
		return true
	}
	if response == nil {
		return false
	}
	switch {
	case response.StatusCode == http.StatusRequestTimeout,
		response.StatusCode == http.StatusTooManyRequests:
		return true
	case response.StatusCode >= 500:
		return true
	}
	return false
}

// backoff computes the sleep before the next attempt: exponential growth
// from BaseDelay with full jitter, superseded by a Retry-After hint when
// the server sends one.
func (t *retryTransport) backoff(attempt int, response *http.Response) time.Duration {
	if response != nil {
		if hint := retryAfter(response.Header.Get("Retry-After")); hint > 0 {
			return hint
		}
	}
	delay := min(t.config.BaseDelay<<(attempt-1), t.config.MaxDelay)
	if delay <= 0 {
		return 0
	}
	// Full jitter: uniform in [0, delay], avoiding synchronized retries.
	return time.Duration(rand.Int63n(int64(delay) + 1))
}

// retryAfter parses the Retry-After header's seconds form.
func retryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
